package server

import (
	"encoding/json"
	"net/http"
)

// healthResponse is the small JSON body returned by the operational endpoints.
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealthz reports process liveness. It returns 200 as long as the process
// can serve requests, independent of downstream dependency state.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// handleReadyz reports whether the gateway is ready to take traffic. It returns
// 503 until SetReady(true) has been called, so load balancers hold traffic
// until startup wiring completes.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
