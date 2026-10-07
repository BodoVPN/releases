package engine

import (
	"encoding/json"
	"errors"
	"fmt"

	libXray "github.com/xtls/libxray"
)

// The methods Invoke answers itself. Every other method goes to libXray unchanged.
const (
	MethodRunXray       = "runXray"
	MethodStopXray      = "stopXray"
	MethodGetXrayState  = "getXrayState"
	MethodQueryStats    = "queryStats"
	MethodPingBatchWarm = "pingBatchWarm"
	MethodBuildInfo     = "buildInfo"
)

// libXray methods that build a core of their own; they wait for the tunnel's core to stop.
const (
	libXrayTestXray  = "testXray"
	libXrayPingBatch = "pingBatch"
)

const (
	apiVersion     = libXray.LibXrayAPIVersion
	maxInvokeBytes = 16 << 20
)

type invokeRequest struct {
	APIVersion int             `json:"apiVersion"`
	Method     string          `json:"method"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type invokeResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Error   string `json:"error"`
}

type runPayload struct {
	XrayJSON string `json:"xrayJson"`
}

type statePayload struct {
	Running bool `json:"running"`
}

// Invoke answers one request in libXray's JSON envelope (API version 3). It runs the tunnel's
// core itself, so queryStats can read it, and adds pingBatchWarm and buildInfo.
func Invoke(requestJSON string) string {
	if len(requestJSON) > maxInvokeBytes {
		return encode(nil, fmt.Errorf("invoke request exceeds the %d MiB size limit", maxInvokeBytes>>20))
	}
	var request invokeRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return encode(nil, err)
	}
	if request.APIVersion != apiVersion {
		return encode(nil, errors.New("unsupported apiVersion"))
	}
	switch request.Method {
	case MethodRunXray:
		payload, err := decodePayload[runPayload](request.Payload)
		if err != nil {
			return encode(nil, err)
		}
		return encodeEmpty(tunnel.run(payload.XrayJSON))
	case MethodStopXray:
		return encodeEmpty(tunnel.stop())
	case MethodGetXrayState:
		return encode(statePayload{Running: tunnel.running()}, nil)
	case MethodQueryStats:
		counters, err := tunnel.counters()
		return encode(counters, err)
	case MethodPingBatchWarm:
		return PingBatchWarmJSON(string(request.Payload), nil)
	case MethodBuildInfo:
		return encode(ReadBuildInfo(), nil)
	case libXrayTestXray, libXrayPingBatch:
		return tunnel.whileStopped(request.Method, func() string { return libXray.Invoke(requestJSON) })
	default:
		return libXray.Invoke(requestJSON)
	}
}

func decodePayload[T any](payload json.RawMessage) (T, error) {
	var value T
	if len(payload) == 0 {
		return value, nil
	}
	err := json.Unmarshal(payload, &value)
	return value, err
}

func encode(data any, err error) string {
	response := invokeResponse{Success: err == nil, Data: data}
	if err != nil {
		response.Data = nil
		response.Error = err.Error()
	}
	raw, marshalErr := json.Marshal(&response)
	if marshalErr != nil {
		return failureJSON("failed to encode response")
	}
	if len(raw) > maxInvokeBytes {
		return failureJSON(fmt.Sprintf("invoke response exceeds the %d MiB size limit", maxInvokeBytes>>20))
	}
	return string(raw)
}

func encodeEmpty(err error) string {
	return encode(struct{}{}, err)
}

func failureJSON(message string) string {
	raw, err := json.Marshal(&invokeResponse{Error: message})
	if err != nil {
		return `{"success":false,"data":null,"error":"failed to encode response"}`
	}
	return string(raw)
}
