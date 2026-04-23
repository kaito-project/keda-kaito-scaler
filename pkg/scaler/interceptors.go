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
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// gRPC server metrics exposed on the controller-runtime metrics registry.
// Naming follows the grpc_server_* convention used by the wider ecosystem so
// that existing Grafana dashboards keep working.
var (
	grpcServerHandled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_server_handled_total",
			Help: "Total number of RPCs completed on the server, regardless of success or failure.",
		},
		[]string{"grpc_type", "grpc_method", "grpc_code"},
	)

	grpcServerLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_server_handling_seconds",
			Help:    "Histogram of response latency (seconds) of gRPC that had been handled by the server.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"grpc_type", "grpc_method"},
	)

	grpcServerPanics = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_server_panics_total",
			Help: "Total number of panics recovered by the gRPC recovery interceptor.",
		},
		[]string{"grpc_method"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(grpcServerHandled, grpcServerLatency, grpcServerPanics)
}

// recoveryUnaryInterceptor converts panics in handlers into Internal errors so
// a single broken RPC cannot take down the whole server.
func recoveryUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				grpcServerPanics.WithLabelValues(info.FullMethod).Inc()
				klog.ErrorS(nil, "recovered from panic in gRPC handler",
					"method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "panic: %v", r)
			}
		}()
		return handler(ctx, req)
	}
}

// recoveryStreamInterceptor mirrors recoveryUnaryInterceptor for streaming RPCs.
func recoveryStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				grpcServerPanics.WithLabelValues(info.FullMethod).Inc()
				klog.ErrorS(nil, "recovered from panic in gRPC stream handler",
					"method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "panic: %v", r)
			}
		}()
		return handler(srv, ss)
	}
}

// metricsUnaryInterceptor records RPC latency and outcome on the
// controller-runtime Prometheus registry. It also emits a V(4) access log line.
func metricsUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)
		code := status.Code(err)

		grpcServerHandled.WithLabelValues("unary", info.FullMethod, code.String()).Inc()
		grpcServerLatency.WithLabelValues("unary", info.FullMethod).Observe(elapsed.Seconds())

		klog.V(4).InfoS("gRPC unary call",
			"method", info.FullMethod, "code", code.String(), "duration", elapsed)
		return resp, err
	}
}

// metricsStreamInterceptor is the streaming counterpart of metricsUnaryInterceptor.
func metricsStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		elapsed := time.Since(start)
		code := status.Code(err)

		grpcServerHandled.WithLabelValues("stream", info.FullMethod, code.String()).Inc()
		grpcServerLatency.WithLabelValues("stream", info.FullMethod).Observe(elapsed.Seconds())

		klog.V(4).InfoS("gRPC stream call",
			"method", info.FullMethod, "code", code.String(), "duration", elapsed)
		return err
	}
}
