// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scaler

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// Well-known service names recognised by HealthServer.Check. kubelet's gRPC
// probe passes the `service` field straight through, so probes can target
// liveness or readiness semantics independently.
const (
	// HealthServiceLiveness always returns SERVING while the process is up;
	// failing this signal tells kubelet to restart the container.
	HealthServiceLiveness = "liveness"
	// HealthServiceReadiness returns SERVING only once every registered
	// readiness check passes (certs loaded, dependencies reachable, ...).
	HealthServiceReadiness = "readiness"
)

// ReadinessCheck reports readiness of a single subsystem. Returning nil means
// "ready"; any error means "not ready yet" and the reason is logged at V(4).
type ReadinessCheck func(ctx context.Context) error

// HealthServer implements grpc.health.v1.Health with separate liveness and
// readiness semantics:
//
//   - service == "" or HealthServiceReadiness: all registered readiness checks
//     must pass, otherwise NOT_SERVING (with the failing check's error logged).
//   - service == HealthServiceLiveness: always SERVING; the gRPC server being
//     able to answer is itself proof of liveness.
//   - any other service name returns NotFound per the grpc.health.v1 spec.
//
// Readiness checks may be added before or after registration via
// AddReadinessCheck; they are guarded by an RWMutex and run with the caller's
// context (kubelet applies its own timeout). Checks run sequentially because
// the set is expected to be small (cert readiness, cache sync, ...).
type HealthServer struct {
	healthpb.UnimplementedHealthServer

	mu     sync.RWMutex
	checks map[string]ReadinessCheck
}

// NewHealthServer returns an empty HealthServer. Callers typically follow up
// with AddReadinessCheck for each subsystem whose readiness should gate the
// kubelet readinessProbe.
func NewHealthServer() *HealthServer {
	return &HealthServer{checks: make(map[string]ReadinessCheck)}
}

// AddReadinessCheck registers or replaces a named readiness probe. name is
// used only for logging — failures are reported via the standard
// grpc.health.v1.Health NOT_SERVING status.
func (h *HealthServer) AddReadinessCheck(name string, check ReadinessCheck) {
	if check == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = check
}

// ChannelReadinessCheck turns a "ready when closed" signal channel into a
// ReadinessCheck. This is the common shape emitted by cert-controller and
// similar producers.
func ChannelReadinessCheck(ready <-chan struct{}) ReadinessCheck {
	return func(context.Context) error {
		select {
		case <-ready:
			return nil
		default:
			return fmt.Errorf("not ready")
		}
	}
}

// Check implements grpc.health.v1.Health.Check.
func (h *HealthServer) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	service := req.GetService()
	klog.V(6).InfoS("gRPC health check", "service", service)

	switch service {
	case HealthServiceLiveness:
		return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
	case "", HealthServiceReadiness:
		return h.readinessResponse(ctx), nil
	default:
		return nil, status.Errorf(codes.NotFound, "unknown service %q", service)
	}
}

// Watch is intentionally not implemented. kubelet's gRPC probe only calls
// Check, and the streaming semantics are not needed.
func (h *HealthServer) Watch(_ *healthpb.HealthCheckRequest, _ healthpb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

// readinessResponse evaluates every registered check under RLock and returns
// NOT_SERVING on the first failure. Check names are iterated in sorted order
// so that logs are stable across calls.
func (h *HealthServer) readinessResponse(ctx context.Context) *healthpb.HealthCheckResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()

	names := make([]string, 0, len(h.checks))
	for name := range h.checks {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := h.checks[name](ctx); err != nil {
			klog.V(4).InfoS("readiness check failed", "name", name, "err", err)
			return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_NOT_SERVING}
		}
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}
}
