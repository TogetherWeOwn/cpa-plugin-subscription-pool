//go:build linux && cgo

// Package main implements the subscription-pool CLIProxyAPI plugin skeleton.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	_ = host
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	payload := C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	var raw []byte
	var err error
	switch C.GoString(method) {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		raw, err = lifecycle(payload)
	case pluginabi.MethodSchedulerPick:
		raw, err = schedulerPick(payload)
	case pluginabi.MethodUsageHandle:
		raw, err = usageHandle(payload)
	case pluginabi.MethodManagementRegister:
		raw, err = okEnvelope(runtimeState.ManagementRegistration())
	case pluginabi.MethodManagementHandle:
		raw, err = managementHandle(payload)
	case pluginabi.MethodPluginShutdown:
		err = runtimeState.Shutdown()
		if err == nil {
			raw, err = okEnvelope(struct{}{})
		}
	default:
		raw = errorEnvelope("unknown_method", "method is not implemented by this plugin")
	}
	if err != nil {
		writeResponse(response, errorEnvelopeFor(err))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

// lifecycle applies a register or reconfigure request. A rejected config
// still answers with a valid registration: the host drops a plugin whose
// register or reconfigure fails, which would hand scheduling of the managed
// credentials back to the host. The plugin stays registered instead, the
// coordinator fails every pick it recognizes closed until a valid config
// arrives, and the management status route reports the rejection.
func lifecycle(payload []byte) ([]byte, error) {
	request, err := decodeLifecycle(payload)
	if err != nil {
		_ = runtimeState.RecordError(fmt.Errorf("lifecycle request body is invalid"))
	} else {
		_ = runtimeState.Reconfigure(request.ConfigYAML)
	}
	return okEnvelope(pluginRegistration())
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func decodeLifecycle(payload []byte) (lifecycleRequest, error) {
	var lifecycle lifecycleRequest
	if len(payload) == 0 {
		return lifecycle, fmt.Errorf("empty lifecycle request")
	}
	if err := json.Unmarshal(payload, &lifecycle); err != nil {
		return lifecycleRequest{}, err
	}
	return lifecycle, nil
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	_ = length
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	_ = runtimeState.Shutdown()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
