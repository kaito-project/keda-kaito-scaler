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

// Package runnable provides small helpers for wrapping low-level servers
// (for example, gRPC servers) into controller-runtime manager.Runnable
// so that their lifecycle is managed alongside the controller manager.
package runnable

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// GRPCServer converts the given gRPC server into a manager.Runnable. The name
// is used for logging and can be any human-readable identifier.
//
// When the provided context is cancelled, the gRPC server is stopped via
// GracefulStop. grpc.ErrServerStopped is treated as a normal shutdown and is
// not returned as an error.
func GRPCServer(name string, srv *grpc.Server, port int) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		log := ctrl.Log.WithValues("name", name)
		log.Info("gRPC server starting")

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return fmt.Errorf("gRPC server %q failed to listen: %w", name, err)
		}
		log.Info("gRPC server listening", "port", port)

		// Terminate the server when the context is cancelled. doneCh makes
		// sure the goroutine does not leak once Serve returns for other
		// reasons (e.g. listener error).
		doneCh := make(chan struct{})
		defer close(doneCh)
		go func() {
			select {
			case <-ctx.Done():
				log.Info("gRPC server shutting down")
				srv.GracefulStop()
			case <-doneCh:
			}
		}()

		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("gRPC server %q failed: %w", name, err)
		}
		log.Info("gRPC server terminated")
		return nil
	})
}
