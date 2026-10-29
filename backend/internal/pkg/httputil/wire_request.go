package httputil

import (
	"context"
	"net/http"
	"net/url"
)

type wireRequestKey struct{}

// WireRequest retains the first read before any account or compatibility rewrite.
// It is request-scoped and must never be stored in usage records.
type WireRequest struct {
	Header http.Header
	URL    url.URL
	Body   []byte
	Plain  []byte
	Read   bool
}

func CaptureWireRequest(req *http.Request) *http.Request {
	if req == nil || OriginalWireRequest(req) != nil {
		return req
	}
	capture := &WireRequest{Header: req.Header.Clone(), URL: *req.URL}
	return req.WithContext(context.WithValue(req.Context(), wireRequestKey{}, capture))
}

func OriginalWireRequest(req *http.Request) *WireRequest {
	if req == nil {
		return nil
	}
	value, _ := req.Context().Value(wireRequestKey{}).(*WireRequest)
	return value
}

func observeWireRequest(req *http.Request, wire, plain []byte) {
	capture := OriginalWireRequest(req)
	if capture == nil || capture.Read {
		return
	}
	capture.Body = wire
	if plain != nil {
		capture.Plain, capture.Read = plain, true
	}
}
