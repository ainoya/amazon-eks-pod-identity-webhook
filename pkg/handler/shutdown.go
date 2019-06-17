/*
  Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.

  Licensed under the Apache License, Version 2.0 (the "License").
  You may not use this file except in compliance with the License.
  A copy of the License is located at

      http://www.apache.org/licenses/LICENSE-2.0

  or in the "license" file accompanying this file. This file is distributed
  on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
  express or implied. See the License for the specific language governing
  permissions and limitations under the License.
*/

package handler

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog"
)

var term = syscall.SIGTERM

// ShutdownOnTerm will wait for SIGTERM or SIGINT and gracefully shuts down the
// http server or kill it after the specified timeout
func ShutdownOnTerm(server *http.Server, timeout time.Duration) {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, term)

	go func() {
		<-c
		klog.Infof("Received SIGTERM/SIGINT. Beginning shutdown")
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != http.ErrServerClosed {
			<-ctx.Done()
			klog.Errorf("Error shutting server down: %v", err)
			if err := server.Close(); err != nil {
				klog.Fatalf("Error closing server: %v", err)
			}
		}
	}()
}
