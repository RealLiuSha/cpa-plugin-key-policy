//go:build cshared

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

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
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

// cgo cannot call C function pointers directly. Static linkage keeps these
// helpers legal in a preamble that is also copied into the export stubs.
static inline int cliproxy_call_host(cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static inline void cliproxy_free_host_buffer(cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	_ "time/tzdata"
	"unsafe"

	"cpa-key-policy/internal/plugin"
)

var app = plugin.NewApp()

// hostAPI is the host function table callbacks go through: the one passed to
// the newest cliproxy_plugin_init whose client CPA has not shut down yet.
// CPA v8.0.31 (355fd10e) and later keep every table allocated for the life
// of the process; v8.0.13 frees one right after its cliproxyPluginShutdown
// returns and then dlcloses the library, so no table may be used once its
// shutdown has returned.
var hostAPI atomic.Pointer[C.cliproxy_host_api]

// hostTables holds the table of every client not shut down yet, newest last.
// CPA HEAD maps a library whose bytes already run in the process to the
// resident runtime, so a hot replacement by the same bytes calls init again
// here, with a new table and callback id. When CPA discards that load (its
// register failed or a newer apply superseded it), it shuts the new client
// down, deletes its callback id and rolls back to the older client, which
// this runtime then serves through the older table. The shutdown function
// does not say which client it closes; CPA discards the newest first, so
// shutdown pops the newest table. CPA v8.0.13 runs one client per runtime,
// and the stack is empty once it is shut down.
var (
	hostTablesMu sync.Mutex
	hostTables   []*C.cliproxy_host_api
)

func pushHostTable(host *C.cliproxy_host_api) {
	hostTablesMu.Lock()
	defer hostTablesMu.Unlock()
	hostTables = append(hostTables, host)
	hostAPI.Store(host)
}

func popHostTable() {
	hostTablesMu.Lock()
	defer hostTablesMu.Unlock()
	if len(hostTables) == 0 {
		return
	}
	hostTables[len(hostTables)-1] = nil
	hostTables = hostTables[:len(hostTables)-1]
	if len(hostTables) == 0 {
		hostAPI.Store(nil)
		return
	}
	hostAPI.Store(hostTables[len(hostTables)-1])
}

func main() {}

// hostCaller performs host callbacks through the stored function table.
type hostCaller struct{}

func (hostCaller) Call(method string, request []byte) ([]byte, error) {
	host := hostAPI.Load()
	if host == nil {
		return nil, errors.New("host callbacks are unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		defer C.free(cRequest)
	}
	var response C.cliproxy_buffer
	status := C.cliproxy_call_host(host, cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	C.cliproxy_free_host_buffer(host, response.ptr, response.len)
	if status != 0 {
		return out, errors.New("host callback " + method + " failed")
	}
	return out, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	// Pushed even when nil, so each shutdown pops its own init's table.
	pushHostTable(host)
	if host != nil {
		app.SetHost(hostCaller{})
	}
	api.abi_version = C.uint32_t(plugin.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, plugin.ErrorEnvelope("invalid_method", "method is required", 400))
		return 1
	}
	// The request is read in place: copying it would double the memory of
	// every multi-megabyte body. The host frees it when this call returns,
	// and HandleMethod keeps nothing of it.
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen))
	}
	raw, err := app.HandleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, plugin.ErrorEnvelope("plugin_error", err.Error(), 500))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// A panic here would end the CPA process that is shutting the plugin
	// down.
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("cpa-key-policy: shutdown panicked: %v", recovered)
		}
	}()
	// Popped after Shutdown, also when it panics: the closing client's own
	// lines still try its table (CPA has closed that instance, so they fall
	// back to stderr), and no table outlives its shutdown.
	defer popHostTable()
	app.Shutdown()
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
