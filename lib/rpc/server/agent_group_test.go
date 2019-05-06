/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"bytes"
	"io/ioutil"
	"time"

	"github.com/gravitational/gravity/lib/rpc/proxy"

	"github.com/cenkalti/backoff"
	"github.com/gravitational/trace"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	. "gopkg.in/check.v1"
)

func (_ *S) TestAgentGroupConnectError(c *C) {
	creds := TestClientCredentials(c)
	watchCh := make(chan WatchEvent, 1)
	checkTimeout := 100 * time.Millisecond
	config := AgentGroupConfig{WatchCh: watchCh, HealthCheckTimeout: checkTimeout}
	nonRoutableAddr := "198.51.100.1:6767"
	peers := []Peer{&remotePeer{addr: nonRoutableAddr, creds: creds}}
	group, err := NewAgentGroup(config, peers)
	c.Assert(err, IsNil)
	group.Start()
	defer withTestCtx(group.Close)

	select {
	case <-watchCh:
		c.Error("unpexpected connect")
	case <-time.After(checkTimeout):
	}
}

func (r *S) TestAgentGroupExecutesCommandsRemotety(c *C) {
	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()
	creds := TestCredentials(c)
	store := newPeerStore()
	l := listen(c)
	log := r.WithField("test", "AgentGroupExecutesCommandsRemotety")
	srv, err := New(Config{
		FieldLogger:     log.WithField("from", l.Addr()),
		Credentials:     creds,
		PeerStore:       store,
		Listener:        l,
		commandExecutor: testCommand{"server output"},
	})
	c.Assert(err, IsNil)
	go srv.Serve()
	defer withTestCtx(srv.Stop)

	serverAddr := srv.Addr().String()
	p1 := r.newPeer(c, PeerConfig{Config: Config{Listener: listen(c)}}, serverAddr, log)
	go p1.Serve()
	defer withTestCtx(p1.Stop)

	p2 := r.newPeer(c, PeerConfig{Config: Config{Listener: listen(c)}}, serverAddr, log)
	go p2.Serve()
	defer withTestCtx(p2.Stop)

	c.Assert(store.expect(ctx, 2), IsNil)

	checkTimeout := 100 * time.Millisecond
	watchCh := make(chan WatchEvent, 2)
	config := AgentGroupConfig{
		FieldLogger:        log.WithField(trace.Component, "agent.group"),
		WatchCh:            watchCh,
		HealthCheckTimeout: checkTimeout,
	}
	group, err := NewAgentGroup(config, store.getPeers())
	c.Assert(err, IsNil)
	group.Start()
	defer withTestCtx(group.Close)

	timeout := time.After(1 * time.Minute)
	for i := 0; i < 2; i++ {
		select {
		case <-watchCh:
		case <-timeout:
			c.Error("failed to wait for reconnect")
		}
	}

	var buf bytes.Buffer
	err = group.WithContext(ctx, p2.Addr().String()).Command(ctx, log, &buf, "test")
	c.Assert(err, IsNil)
	c.Assert(buf.String(), DeepEquals, "test output")
}

func (r *S) TestAgentGroupReconnects(c *C) {
	creds := TestCredentials(c)
	store := newPeerStore()
	listener := listen(c)
	log := r.WithField("test", "AgentGroupReconnects")
	srv, err := New(Config{
		FieldLogger:     log.WithField("server", listener.Addr()),
		Credentials:     creds,
		PeerStore:       store,
		Listener:        listener,
		commandExecutor: testCommand{"server output"},
	})
	c.Assert(err, IsNil)
	go srv.Serve()
	defer withTestCtx(srv.Stop)

	upstream := listen(c)
	local := listen(c)
	proxyAddr := local.Addr().String()
	proxyLink := proxy.New(proxy.NetLink{Local: local, Upstream: upstream.Addr().String()}, log)
	proxyLink.Start()

	serverAddr := srv.Addr().String()
	p1 := r.newPeer(c, PeerConfig{Config: Config{Listener: listen(c)}}, serverAddr, log)
	go p1.Serve()
	defer withTestCtx(p1.Stop)
	// Have peer 2 go through a proxy so its connection can be manipulated
	p2 := r.newPeer(c, PeerConfig{Config: Config{Listener: upstream}, proxyAddr: proxyAddr}, serverAddr, log)
	go p2.Serve()
	defer withTestCtx(p2.Stop)

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	c.Assert(store.expect(ctx, 2), IsNil)
	cancel()

	checkTimeout := 100 * time.Millisecond
	watchCh := make(chan WatchEvent, 3)
	config := AgentGroupConfig{
		FieldLogger:        log.WithField(trace.Component, "agent.group"),
		WatchCh:            watchCh,
		HealthCheckTimeout: checkTimeout,
	}

	doneCh := make(chan struct{})
	go func() {
		timeoutCh := time.After(5 * time.Second)
		for i := 0; i < 2; i++ {
			select {
			case <-watchCh:
			case <-timeoutCh:
				c.Error("timeout waiting for reconnect")
			}
		}
		close(doneCh)
	}()

	group, err := NewAgentGroup(config, store.getPeers())
	c.Assert(err, IsNil)
	group.Start()
	defer withTestCtx(group.Close)

	// Wait for reconnects
	<-doneCh

	// Drop connection to peer 2
	proxyLink.Stop()
	// Give the transport enough time to fail. If the interval between reconnects
	// is negligible, the transport might recover and reconnect
	// to the second instance of the proxy bypassing the failed health check.
	time.Sleep(checkTimeout)

	ctx, cancel = context.WithTimeout(context.TODO(), 1*time.Second)
	err = group.WithContext(ctx, proxyAddr).Command(ctx, log, ioutil.Discard, "test")
	cancel()
	c.Assert(err, Not(IsNil))
	errorCode := grpc.Code(trace.Unwrap(err))
	assertCodeOneOf(c, errorCode, codes.Unavailable, codes.Unknown)

	// Restore connection to peer 2
	local = listenAddr(proxyAddr, c)
	proxyLink = proxy.New(proxy.NetLink{Local: local, Upstream: upstream.Addr().String()}, log)
	proxyLink.Start()
	defer proxyLink.Stop()

	select {
	case update := <-watchCh:
		c.Assert(update.Error, IsNil)
		if update.Peer.Addr() != proxyAddr {
			c.Errorf("unknown peer %v", update.Peer)
		}
		// Reconnected
	case <-time.After(5 * time.Second):
		c.Error("timeout waiting for reconnect")
	}

	var buf bytes.Buffer
	ctx, cancel = context.WithTimeout(context.TODO(), 1*time.Second)
	err = group.WithContext(ctx, proxyAddr).Command(ctx, log, &buf, "test")
	cancel()
	c.Assert(err, IsNil)
	c.Assert(buf.String(), DeepEquals, "test output")
}

func (r *S) TestAgentGroupRemovesPeerItCannotReconnect(c *C) {
	creds := TestCredentials(c)
	store := newPeerStore()
	l := listen(c)
	log := r.WithField("test", "AgentGroupRemovesPeerItCannotReconnect")
	srv, err := New(Config{
		FieldLogger:      log.WithField("server", l.Addr()),
		Credentials:      creds,
		PeerStore:        store,
		Listener:         l,
		ReconnectTimeout: 1 * time.Second,
	})
	c.Assert(err, IsNil)
	go srv.Serve()
	defer withTestCtx(srv.Stop)

	upstream := listen(c)
	local := listen(c)
	proxyAddr := local.Addr().String()
	proxyLink := proxy.New(proxy.NetLink{Local: local, Upstream: upstream.Addr().String()}, log)
	proxyLink.Start()

	serverAddr := srv.Addr().String()
	// Have peer go through a proxy so its connection can be manipulated
	p := r.newPeer(c, PeerConfig{Config: Config{Listener: upstream}, proxyAddr: proxyAddr}, serverAddr, log)
	go p.Serve()
	defer withTestCtx(p.Stop)

	ctx, cancel := context.WithTimeout(context.TODO(), 1*time.Second)
	c.Assert(store.expect(ctx, 1), IsNil)
	cancel()

	checkTimeout := 1 * time.Second
	watchCh := make(chan WatchEvent, 2)
	config := AgentGroupConfig{
		FieldLogger:        log.WithField(trace.Component, "agent.group"),
		WatchCh:            watchCh,
		HealthCheckTimeout: checkTimeout,
		ReconnectStrategy: ReconnectStrategy{
			// Do not try to reconnect
			Backoff: func() backoff.BackOff { return &backoff.StopBackOff{} },
		},
	}
	group, err := NewAgentGroup(config, store.getPeers())
	c.Assert(err, IsNil)
	group.Start()
	defer withTestCtx(group.Close)

	select {
	case resp := <-watchCh:
		log.Infof("Reconnect response: %v.", resp)
		c.Assert(resp.Client, Not(IsNil))
	case <-time.After(5 * time.Second):
		c.Error("timeout waiting for reconnect")
	}

	// Drop connection to peer
	proxyLink.Stop()
	// Give the transport enough time to fail. If the interval between reconnects
	// is negligible, the transport might recover and reconnect
	// to the second instance of the proxy bypassing the failed health check.
	time.Sleep(checkTimeout)

	select {
	case resp := <-watchCh:
		log.Infof("Reconnect failure response: %v.", resp)
		c.Assert(resp.Error, Not(IsNil))
		c.Assert(group.NumPeers(), Equals, 0)
	case <-time.After(5 * time.Second):
		c.Error("timeout waiting for reconnect failure")
	}
}

func assertCodeOneOf(c *C, actual codes.Code, expected ...codes.Code) {
	for _, code := range expected {
		if code == actual {
			return
		}
	}
	c.Errorf("code %v did not match any of %v", actual, expected)
}
