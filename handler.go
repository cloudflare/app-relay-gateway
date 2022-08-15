// Copyright (c) 2022 Cloudflare, Inc. All rights reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"

	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"

	"github.com/chris-wood/ohttp-go"
	"google.golang.org/protobuf/proto"
)

var ConfigMismatchError = errors.New("Configuration mismatch")
var EncapsulationError = errors.New("Encapsulation error")
var TargetForbiddenError = errors.New("Target forbidden")

// EncapsulationHandler handles OHTTP encapsulated requests and produces OHTTP encapsulated responses.
type EncapsulationHandler interface {
	// Handle processes an OHTTP encapsulated request and produces an OHTTP encapsulated response, or an error
	// if any part of the encapsulation or decapsulation process fails.
	Handle(outerRequest *http.Request, encapRequest ohttp.EncapsulatedRequest) (ohttp.EncapsulatedResponse, error)
}

// DefaultEncapsulationHandler is an EncapsulationHandler that uses a default OHTTP gateway to decapsulate
// requests, pass them to an AppContentHandler to produce a response for encapsulation, and encapsulates the
// response.
type DefaultEncapsulationHandler struct {
	keyID      uint8
	gateway    ohttp.Gateway
	appHandler AppContentHandler
}

// Handle attempts to decapsulate the incoming encapsulated request and, if successful, passes the
// corresponding application payload to the AppContentHandler for producing a response to encapsulate
// and return.
func (h DefaultEncapsulationHandler) Handle(outerRequest *http.Request, encapsulatedReq ohttp.EncapsulatedRequest) (ohttp.EncapsulatedResponse, error) {
	if encapsulatedReq.KeyID != h.keyID {
		return ohttp.EncapsulatedResponse{}, ConfigMismatchError
	}

	binaryRequest, context, err := h.gateway.DecapsulateRequest(encapsulatedReq)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, EncapsulationError
	}

	binaryResponse, err := h.appHandler.Handle(binaryRequest)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, err
	}

	encapsulatedResponse, err := context.EncapsulateResponse(binaryResponse)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, err
	}

	return encapsulatedResponse, nil
}

// MetadataEncapsulationHandler is an EncapsulationHandler that uses a default OHTTP gateway to decapsulate
// requests and return metadata about the encapsulated request context as an encapsulated response. Metadata
// includes, for example, the list of headers carried on the encapsulated request from the client or relay.
type MetadataEncapsulationHandler struct {
	keyID   uint8
	gateway ohttp.Gateway
}

// Handle attempts to decapsulate the incoming encapsulated request and, if successful, foramts
// metadata from the request context, and then encapsulates and returns the result.
func (h MetadataEncapsulationHandler) Handle(outerRequest *http.Request, encapsulatedReq ohttp.EncapsulatedRequest) (ohttp.EncapsulatedResponse, error) {
	if encapsulatedReq.KeyID != h.keyID {
		return ohttp.EncapsulatedResponse{}, ConfigMismatchError
	}

	_, context, err := h.gateway.DecapsulateRequest(encapsulatedReq)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, EncapsulationError
	}

	// XXX(caw): maybe also include the encapsulated request and its plaintext form too?
	binaryResponse, err := httputil.DumpRequest(outerRequest, false)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, err
	}

	encapsulatedResponse, err := context.EncapsulateResponse(binaryResponse)
	if err != nil {
		return ohttp.EncapsulatedResponse{}, err
	}

	return encapsulatedResponse, nil
}

// AppContentHandler processes application-specific request content and produces response content.
type AppContentHandler interface {
	Handle(binaryRequest []byte) ([]byte, error)
}

// EchoAppHandler is an AppContentHandler that returns the application request as the response.
type EchoAppHandler struct {
}

// Handle returns the input request as the response.
func (h EchoAppHandler) Handle(binaryRequest []byte) ([]byte, error) {
	return binaryRequest, nil
}

// ProtoHTTPEncapsulationHandler is an AppContentHandler that parses the application request as
// a protobuf-based HTTP request for resolution with an HttpRequestHandler.
type ProtoHTTPEncapsulationHandler struct {
	httpHandler HttpRequestHandler
}

func (h ProtoHTTPEncapsulationHandler) createWrappedErrorRepsonse(e error, statusCode int32) ([]byte, error) {
	resp := &Response{
		StatusCode: statusCode,
		Body:       []byte(e.Error()),
	}
	respEnc, err := proto.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return respEnc, nil
}

// Handle attempts to parse the application payload as a protobuf-based HTTP request and, if successful,
// translates the result into an equivalent http.Request object to be processed by the handler's HttpRequestHandler.
// The http.Response result from the handler is then translated back into an equivalent protobuf-based HTTP
// response and returned to the caller.
func (h ProtoHTTPEncapsulationHandler) Handle(binaryRequest []byte) ([]byte, error) {
	req := &Request{}
	if err := proto.Unmarshal(binaryRequest, req); err != nil {
		log.Println("Failed to unmarshal protobuf message", err)
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	httpRequest, err := protoHTTPToRequest(req)
	if err != nil {
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	httpResponse, err := h.httpHandler.Handle(httpRequest)
	if err != nil {
		if err == TargetForbiddenError {
			// Return 401 (Unauthorized) in the event the client request was for a
			// Target not on the allow list
			return h.createWrappedErrorRepsonse(err, http.StatusForbidden)
		}
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	protoResponse, err := responseToProtoHTTP(httpResponse)
	if err != nil {
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	return proto.Marshal(protoResponse)
}

// BinaryHTTPAppHandler is an AppContentHandler that parses the application request as
// a binary HTTP request for resolution with an HttpRequestHandler.
type BinaryHTTPAppHandler struct {
	httpHandler HttpRequestHandler
}

func (h BinaryHTTPAppHandler) createWrappedErrorRepsonse(e error, statusCode int) ([]byte, error) {
	resp := &http.Response{
		StatusCode: statusCode,
		Body:       ioutil.NopCloser(bytes.NewBufferString(e.Error())),
	}
	binaryResponse := ohttp.CreateBinaryResponse(resp)
	return binaryResponse.Marshal()
}

// Handle attempts to parse the application payload as a binary HTTP request and, if successful,
// translates the result into an equivalent http.Request object to be processed by the handler's HttpRequestHandler.
// The http.Response result from the handler is then translated back into an equivalent binary HTTP
// response and returned to the caller.
func (h BinaryHTTPAppHandler) Handle(binaryRequest []byte) ([]byte, error) {
	req, err := ohttp.UnmarshalBinaryRequest(binaryRequest)
	if err != nil {
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	resp, err := h.httpHandler.Handle(req)
	if err != nil {
		if err == TargetForbiddenError {
			// Return 401 (Unauthorized) in the event the client request was for a
			// Target not on the allow list
			return h.createWrappedErrorRepsonse(err, http.StatusForbidden)
		}
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	binaryResp := ohttp.CreateBinaryResponse(resp)
	binaryRespEnc, err := binaryResp.Marshal()
	if err != nil {
		return h.createWrappedErrorRepsonse(err, http.StatusInternalServerError)
	}

	return binaryRespEnc, nil
}

// HttpRequestHandler handles HTTP requests to produce responses.
type HttpRequestHandler interface {
	// Handle takes a http.Request and resolves it to produce a http.Response.
	Handle(req *http.Request) (*http.Response, error)
}

// FilteredHttpRequestHandler represents a HttpRequestHandler that restricts
// outbound HTTP requests to an allowed set of targets.
type FilteredHttpRequestHandler struct {
	client         *http.Client
	allowedOrigins map[string]bool
}

// Handle processes HTTP requests to targets that are permitted according to a list of
// allowed targets.
func (h FilteredHttpRequestHandler) Handle(req *http.Request) (*http.Response, error) {
	if h.allowedOrigins != nil {
		_, ok := h.allowedOrigins[req.Host]
		if !ok {
			return nil, TargetForbiddenError
		}
	}

	return h.client.Do(req)
}