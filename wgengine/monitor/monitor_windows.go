// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package monitor

import (
	"context"
	"errors"
	"time"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"tailscale.com/types/logger"
)

var (
	errClosed = errors.New("closed")
)

type eventMessage struct {
	eventType string
}

func (eventMessage) ignore() bool { return false }

type winMon struct {
	logf                  logger.Logf
	ctx                   context.Context
	cancel                context.CancelFunc
	messagec              chan eventMessage
	addressChangeCallback *winipcfg.UnicastAddressChangeCallback
	routeChangeCallback   *winipcfg.RouteChangeCallback

	// noDeadlockTicker exists just to have something scheduled as
	// far as the Go runtime is concerned. Otherwise "tailscaled
	// debug --monitor" thinks it's deadlocked with nothing to do,
	// as Go's runtime doesn't know about callbacks registered with
	// Windows.
	noDeadlockTicker *time.Ticker
}

func newOSMon(logf logger.Logf, _ *Mon) (osMon, error) {
	m := &winMon{
		logf:             logf,
		messagec:         make(chan eventMessage, 1),
		noDeadlockTicker: time.NewTicker(5000 * time.Hour), // arbitrary
	}

	var err error
	m.addressChangeCallback, err = winipcfg.RegisterUnicastAddressChangeCallback(m.unicastAddressChanged)
	if err != nil {
		m.logf("winipcfg.RegisterUnicastAddressChangeCallback error: %v", err)
		return nil, err
	}

	m.routeChangeCallback, err = winipcfg.RegisterRouteChangeCallback(m.routeChanged)
	if err != nil {
		m.addressChangeCallback.Unregister()
		m.logf("winipcfg.RegisterRouteChangeCallback error: %v", err)
		return nil, err
	}

	m.ctx, m.cancel = context.WithCancel(context.Background())

	return m, nil
}

func (m *winMon) Close() (ret error) {
	m.cancel()
	m.noDeadlockTicker.Stop()

	if m.addressChangeCallback != nil {
		if err := m.addressChangeCallback.Unregister(); err != nil {
			m.logf("addressChangeCallback.Unregister error: %v", err)
			ret = err
		} else {
			m.addressChangeCallback = nil
		}
	}

	if m.routeChangeCallback != nil {
		if err := m.routeChangeCallback.Unregister(); err != nil {
			m.logf("routeChangeCallback.Unregister error: %v", err)
			ret = err
		} else {
			m.routeChangeCallback = nil
		}
	}

	return
}

func (m *winMon) Receive() (message, error) {
	if m.ctx.Err() != nil {
		m.logf("Receive call on closed monitor")
		return nil, errClosed
	}

	t0 := time.Now()

	select {
	case msg := <-m.messagec:
		m.logf("got windows change event after %v: evt=%s", time.Since(t0).Round(time.Millisecond), msg.eventType)
		return msg, nil
	case <-m.ctx.Done():
		return nil, errClosed
	}
}

// unicastAddressChanged is the callback we register with Windows to call when unicast address changes.
func (m *winMon) unicastAddressChanged(_ winipcfg.MibNotificationType, _ *winipcfg.MibUnicastIPAddressRow) {
	// start a goroutine to finish our work, to return to Windows out of this callback
	go m.somethingChanged("addr")
}

// routeChanged is the callback we register with Windows to call when route changes.
func (m *winMon) routeChanged(_ winipcfg.MibNotificationType, _ *winipcfg.MibIPforwardRow2) {
	// start a goroutine to finish our work, to return to Windows out of this callback
	go m.somethingChanged("route")
}

// somethingChanged gets called from OS callbacks whenever address or route changes.
func (m *winMon) somethingChanged(evt string) {
	select {
	case <-m.ctx.Done():
		return
	case m.messagec <- eventMessage{eventType: evt}:
		return
	}
}
