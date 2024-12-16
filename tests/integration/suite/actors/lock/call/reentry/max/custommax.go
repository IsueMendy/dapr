/*
Copyright 2024 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implieh.
See the License for the specific language governing permissions and
limitations under the License.
*/

package max

import (
	"context"
	nethttp "net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rtv1 "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/process/daprd/actors"
	"github.com/dapr/dapr/tests/integration/suite"
	"github.com/dapr/kit/concurrency/slice"
	"github.com/dapr/kit/ptr"
)

func init() {
	suite.Register(new(custommax))
}

type custommax struct {
	app      *actors.Actors
	called   slice.Slice[string]
	rid      atomic.Pointer[string]
	holdCall chan struct{}
}

func (c *custommax) Setup(t *testing.T) []framework.Option {
	c.called = slice.New[string]()
	c.holdCall = make(chan struct{})

	handler := func(_ nethttp.ResponseWriter, r *nethttp.Request) {
		if r.Method == nethttp.MethodDelete {
			return
		}
		if c.rid.Load() == nil {
			c.rid.Store(ptr.Of(r.Header.Get("Dapr-Reentrancy-Id")))
		}
		c.called.Append(r.URL.Path)
		<-c.holdCall
	}

	c.app = actors.New(t,
		actors.WithActorTypes("abc"),
		actors.WithActorTypeHandler("abc", handler),
		actors.WithReentry(true),
		actors.WithReentryMaxDepth(23),
	)
	return []framework.Option{
		framework.WithProcesses(c.app),
	}
}

func (c *custommax) Run(t *testing.T, ctx context.Context) {
	c.app.WaitUntilRunning(t, ctx)

	client := c.app.GRPCClient(t, ctx)

	errCh := make(chan error)
	go func() {
		_, err := client.InvokeActor(ctx, &rtv1.InvokeActorRequest{
			ActorType: "abc",
			ActorId:   "123",
			Method:    "foo",
		})
		errCh <- err
	}()

	assert.EventuallyWithT(t, func(col *assert.CollectT) {
		assert.Equal(col, []string{
			"/actors/abc/123/method/foo",
		}, c.called.Slice())
	}, time.Second*10, time.Millisecond*10)

	require.NotNil(t, c.rid.Load())
	id := *(c.rid.Load())

	for range 22 {
		go func() {
			_, err := client.InvokeActor(ctx, &rtv1.InvokeActorRequest{
				ActorType: "abc",
				ActorId:   "123",
				Method:    "foo",
				Metadata:  map[string]string{"Dapr-Reentrancy-Id": id},
			})
			errCh <- err
		}()
	}

	assert.EventuallyWithT(t, func(col *assert.CollectT) {
		assert.Equal(col, 23, c.called.Len())
	}, time.Second*10, time.Millisecond*10)

	_, err := client.InvokeActor(ctx, &rtv1.InvokeActorRequest{
		ActorType: "abc",
		ActorId:   "123",
		Method:    "foo",
		Metadata:  map[string]string{"Dapr-Reentrancy-Id": id},
	})
	require.Error(t, err)
	status, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.ResourceExhausted.String(), status.Code().String())

	for range 23 {
		c.holdCall <- struct{}{}
	}

	for range 23 {
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(time.Second * 5):
			assert.Fail(t, "timeout")
		}
	}
}
