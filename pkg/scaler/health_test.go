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
	"errors"
	"testing"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestHealthServer_Liveness_AlwaysServing(t *testing.T) {
	h := NewHealthServer()
	// Even with failing readiness checks, liveness must stay SERVING.
	h.AddReadinessCheck("fail", func(context.Context) error { return errors.New("boom") })

	resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: HealthServiceLiveness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("liveness status = %v, want SERVING", resp.GetStatus())
	}
}

func TestHealthServer_Readiness_NoChecks(t *testing.T) {
	h := NewHealthServer()
	resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: HealthServiceReadiness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("readiness (empty) status = %v, want SERVING", resp.GetStatus())
	}
}

func TestHealthServer_Readiness_FailPropagates(t *testing.T) {
	h := NewHealthServer()
	h.AddReadinessCheck("ok", func(context.Context) error { return nil })
	h.AddReadinessCheck("bad", func(context.Context) error { return errors.New("not yet") })

	for _, service := range []string{"", HealthServiceReadiness} {
		resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: service})
		if err != nil {
			t.Fatalf("service=%q unexpected error: %v", service, err)
		}
		if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
			t.Fatalf("service=%q status = %v, want NOT_SERVING", service, resp.GetStatus())
		}
	}
}

func TestHealthServer_UnknownService_NotFound(t *testing.T) {
	h := NewHealthServer()
	_, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "nope"})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if got := status.Code(err); got.String() != "NotFound" {
		t.Fatalf("code = %v, want NotFound", got)
	}
}

func TestChannelReadinessCheck(t *testing.T) {
	ch := make(chan struct{})
	check := ChannelReadinessCheck(ch)

	if err := check(context.Background()); err == nil {
		t.Fatal("expected error before channel is closed")
	}
	close(ch)
	if err := check(context.Background()); err != nil {
		t.Fatalf("expected nil after channel close, got %v", err)
	}
}

func TestHealthServer_Readiness_FlipsWhenChannelCloses(t *testing.T) {
	h := NewHealthServer()
	ch := make(chan struct{})
	h.AddReadinessCheck("cert", ChannelReadinessCheck(ch))

	resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: HealthServiceReadiness})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("before close: got %v, want NOT_SERVING", resp.GetStatus())
	}

	close(ch)

	resp, err = h.Check(context.Background(), &healthpb.HealthCheckRequest{Service: HealthServiceReadiness})
	if err != nil {
		t.Fatalf("unexpected error after close: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("after close: got %v, want SERVING", resp.GetStatus())
	}
}
