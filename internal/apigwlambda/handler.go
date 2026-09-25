// Package apigwlambda adapts API Gateway HTTP API v2 proxy events to net/http.
package apigwlambda

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

const maxEventBodyBytes = 10 << 20

type Handler struct {
	HTTP http.Handler
}

func New(handler http.Handler) Handler {
	return Handler{HTTP: handler}
}

// Handle translates API Gateway v2 HTTP events into net/http requests and
// formats the HTTP response in API Gateway's Lambda proxy response shape.
func (h Handler) Handle(ctx context.Context, event events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	if h.HTTP == nil {
		return events.APIGatewayV2HTTPResponse{}, errors.New("HTTP handler is required")
	}
	if event.Version != "2.0" {
		return events.APIGatewayV2HTTPResponse{}, errors.New("API Gateway HTTP API payload version 2.0 is required")
	}
	method := strings.TrimSpace(event.RequestContext.HTTP.Method)
	if method == "" || event.RawPath == "" || !strings.HasPrefix(event.RawPath, "/") {
		return events.APIGatewayV2HTTPResponse{}, errors.New("API Gateway request method and absolute path are required")
	}
	body := []byte(event.Body)
	if event.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{}, errors.New("API Gateway request body is invalid base64")
		}
		body = decoded
	}
	if len(body) > maxEventBodyBytes {
		return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusRequestEntityTooLarge, Headers: map[string]string{"content-type": "application/json"}, Body: `{"error":{"code":"request_too_large","message":"Request body is too large"}}`}, nil
	}

	host := event.Headers["host"]
	if host == "" {
		host = event.RequestContext.DomainName
	}
	if host == "" {
		host = "localhost"
	}
	requestURL, err := url.ParseRequestURI(event.RawPath + "?" + event.RawQueryString)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, errors.New("API Gateway request path is invalid")
	}
	requestURL.Scheme = "https"
	requestURL.Host = host
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, errors.New("create HTTP request from API Gateway event")
	}
	request.Host = host
	for name, value := range event.Headers {
		if !strings.EqualFold(name, "host") {
			request.Header.Set(name, value)
		}
	}
	for _, cookie := range event.Cookies {
		request.Header.Add("Cookie", cookie)
	}
	if event.RequestContext.HTTP.SourceIP != "" {
		request.RemoteAddr = event.RequestContext.HTTP.SourceIP
	}

	recorder := httptest.NewRecorder()
	h.HTTP.ServeHTTP(recorder, request)
	response := events.APIGatewayV2HTTPResponse{
		StatusCode: recorder.Code, Headers: make(map[string]string),
		Body: recorder.Body.String(), IsBase64Encoded: false,
	}
	for name, values := range recorder.Header() {
		if strings.EqualFold(name, "set-cookie") {
			response.Cookies = append(response.Cookies, values...)
			continue
		}
		if len(values) > 0 {
			response.Headers[name] = strings.Join(values, ",")
		}
	}
	return response, nil
}
