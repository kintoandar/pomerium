//go:generate protoc -I ../internal/grpc/authorize/ --go_out=plugins=grpc:../internal/grpc/authorize/ ../internal/grpc/authorize/authorize.proto

package authorize

import (
	"context"
	"net/http"
	"net/url"

	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_service_auth_v2 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v2"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type"
	"github.com/pomerium/pomerium/authorize/evaluator"
	"github.com/pomerium/pomerium/config"
	"github.com/pomerium/pomerium/internal/encoding/jws"
	"github.com/pomerium/pomerium/internal/grpc/authorize"
	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/sessions"
	"github.com/pomerium/pomerium/internal/sessions/cookie"
	"github.com/pomerium/pomerium/internal/telemetry/trace"
	"github.com/pomerium/pomerium/internal/urlutil"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

// IsAuthorized checks to see if a given user is authorized to make a request.
func (a *Authorize) IsAuthorized(ctx context.Context, in *authorize.IsAuthorizedRequest) (*authorize.IsAuthorizedReply, error) {
	ctx, span := trace.StartSpan(ctx, "authorize.grpc.IsAuthorized")
	defer span.End()

	req := &evaluator.Request{
		User:       in.GetUserToken(),
		Header:     cloneHeaders(in.GetRequestHeaders()),
		Host:       in.GetRequestHost(),
		Method:     in.GetRequestMethod(),
		RequestURI: in.GetRequestRequestUri(),
		RemoteAddr: in.GetRequestRemoteAddr(),
		URL:        getFullURL(in.GetRequestUrl(), in.GetRequestHost()),
	}
	return a.pe.IsAuthorized(ctx, req)
}

func (a *Authorize) Check(ctx context.Context, in *envoy_service_auth_v2.CheckRequest) (*envoy_service_auth_v2.CheckResponse, error) {
	log.Info().Interface("in", in).Msg("checking authorization")

	hdrs := getCheckRequestHeaders(in)
	sess, sesserr := a.loadSessionFromCheckRequest(in)
	requestURL := getCheckRequestURL(in)
	req := &evaluator.Request{
		User:       sess,
		Header:     hdrs,
		Host:       in.GetAttributes().GetRequest().GetHttp().GetHost(),
		Method:     in.GetAttributes().GetRequest().GetHttp().GetMethod(),
		RequestURI: requestURL.String(),
		RemoteAddr: in.GetAttributes().GetSource().GetAddress().String(),
		URL:        requestURL.String(),
	}
	log.Info().Interface("request", req).Msg("is authorized???")
	reply, err := a.pe.IsAuthorized(ctx, req)
	if err != nil {
		return nil, err
	}
	log.Info().Interface("reply", reply).Msg("is authorized???")

	if reply.Allow {
		return &envoy_service_auth_v2.CheckResponse{
			Status:       &status.Status{Code: int32(codes.OK), Message: "OK"},
			HttpResponse: &envoy_service_auth_v2.CheckResponse_OkResponse{OkResponse: &envoy_service_auth_v2.OkHttpResponse{}},
		}, nil
	}

	switch sesserr {
	case sessions.ErrExpired, sessions.ErrIssuedInTheFuture, sessions.ErrMalformed, sessions.ErrNoSessionFound, sessions.ErrNotValidYet:
		// redirect to login
	default:
		var msg string
		if sesserr != nil {
			msg = sesserr.Error()
		}
		// all other errors
		return &envoy_service_auth_v2.CheckResponse{
			Status: &status.Status{Code: int32(codes.PermissionDenied), Message: msg},
			HttpResponse: &envoy_service_auth_v2.CheckResponse_DeniedResponse{
				DeniedResponse: &envoy_service_auth_v2.DeniedHttpResponse{
					Status: &envoy_type.HttpStatus{
						Code: envoy_type.StatusCode_Forbidden,
					},
				},
			},
		}, nil
	}

	signinURL := requestURL.ResolveReference(&url.URL{Path: "/.pomerium/sign_in"})
	q := signinURL.Query()
	q.Set(urlutil.QueryRedirectURI, requestURL.String())
	signinURL.RawQuery = q.Encode()
	redirectTo := signinURL.String()

	return &envoy_service_auth_v2.CheckResponse{
		Status: &status.Status{
			Code:    int32(codes.Unauthenticated),
			Message: "unauthenticated",
		},
		HttpResponse: &envoy_service_auth_v2.CheckResponse_DeniedResponse{
			DeniedResponse: &envoy_service_auth_v2.DeniedHttpResponse{
				Status: &envoy_type.HttpStatus{
					Code: envoy_type.StatusCode_Found,
				},
				Headers: []*envoy_api_v2_core.HeaderValueOption{{
					Header: &envoy_api_v2_core.HeaderValue{
						Key:   "Location",
						Value: redirectTo,
					},
				}},
			},
		},
	}, nil
}

func (a *Authorize) loadSessionFromCheckRequest(req *envoy_service_auth_v2.CheckRequest) (string, error) {
	opts := a.currentOptions.Load().(config.Options)

	// used to load and verify JWT tokens signed by the authenticate service
	encoder, err := jws.NewHS256Signer([]byte(opts.SharedKey), opts.AuthenticateURL.Host)
	if err != nil {
		return "", err
	}

	cookieOptions := &cookie.Options{
		Name:     opts.CookieName,
		Domain:   opts.CookieDomain,
		Secure:   opts.CookieSecure,
		HTTPOnly: opts.CookieHTTPOnly,
		Expire:   opts.CookieExpire,
	}

	cookieStore, err := cookie.NewStore(cookieOptions, encoder)
	if err != nil {
		return "", err
	}

	sess, err := cookieStore.LoadSession(&http.Request{
		Header: http.Header(getCheckRequestHeaders(req)),
	})
	return sess, err
}

type protoHeader map[string]*authorize.IsAuthorizedRequest_Headers

func cloneHeaders(in protoHeader) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		newValues := make([]string, len(values.Value))
		copy(newValues, values.Value)
		out[key] = newValues
	}
	return out
}

func getFullURL(rawurl, host string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		u = &url.URL{Path: rawurl}
	}
	if u.Host == "" {
		u.Host = host
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return u.String()
}

func getCheckRequestHeaders(req *envoy_service_auth_v2.CheckRequest) map[string][]string {
	h := make(map[string][]string)
	ch := req.GetAttributes().GetRequest().GetHttp().GetHeaders()
	if ch != nil {
		for k, v := range ch {
			h[http.CanonicalHeaderKey(k)] = []string{v}
		}
	}
	return h
}

func getCheckRequestURL(req *envoy_service_auth_v2.CheckRequest) *url.URL {
	h := req.GetAttributes().GetRequest().GetHttp()
	u := &url.URL{
		Scheme:   h.GetScheme(),
		Host:     h.GetHost(),
		Path:     h.GetPath(),
		RawQuery: h.GetQuery(),
	}
	if h.Headers != nil {
		if fwdProto, ok := h.Headers["x-forwarded-proto"]; ok {
			u.Scheme = fwdProto
		}
	}
	return u
}
