// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cache

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/go-secure-stdlib/reloadutil"
	"github.com/openbao/openbao/command/server"
	"github.com/openbao/openbao/internalshared/configutil"
	"github.com/openbao/openbao/internalshared/listenerutil"
)

type ListenerBundle struct {
	Listener      net.Listener
	TLSConfig     *tls.Config
	CertGetter    listenerutil.ReloadableCertGetter
	TLSReloadFunc reloadutil.ReloadFunc
}

func StartListener(lnConfig *configutil.Listener) (*ListenerBundle, error) {
	addr := lnConfig.Address

	var ln net.Listener
	var err error
	switch lnConfig.Type {
	case "tcp":
		if addr == "" {
			addr = "127.0.0.1:8200"
		}

		bindProto := "tcp"
		// If they've passed 0.0.0.0, we only want to bind on IPv4
		// rather than golang's dual stack default
		if strings.HasPrefix(addr, "0.0.0.0:") {
			bindProto = "tcp4"
		}

		ln, err = net.Listen(bindProto, addr)
		if err != nil {
			return nil, err
		}
		ln = &server.TCPKeepAliveListener{TCPListener: ln.(*net.TCPListener)}

	case "unix":
		var uConfig *listenerutil.UnixSocketsConfig
		if lnConfig.SocketMode != "" &&
			lnConfig.SocketUser != "" &&
			lnConfig.SocketGroup != "" {
			uConfig = &listenerutil.UnixSocketsConfig{
				Mode:  lnConfig.SocketMode,
				User:  lnConfig.SocketUser,
				Group: lnConfig.SocketGroup,
			}
		}
		ln, err = listenerutil.UnixSocketListener(addr, uConfig)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("invalid listener type: %q", lnConfig.Type)
	}

	props := map[string]string{"addr": ln.Addr().String()}
	tlsConf, cg, err := listenerutil.TLSConfig(lnConfig, props, nil, nil)
	if err != nil {
		return nil, err
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}

	cfg := &ListenerBundle{
		Listener:  ln,
		TLSConfig: tlsConf,
	}

	if cg != nil {
		cfg.CertGetter = cg
		cfg.TLSReloadFunc = cg.Reload
	}

	return cfg, nil
}
