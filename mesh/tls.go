package mesh

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync/atomic"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/internal/tlsreload"
	"github.com/rs/zerolog"
)

const meshALPN = "qmux-mesh-v1"

type inboundTLSState struct {
	certificate tls.Certificate
	clientCAs   *x509.CertPool
}

type outboundTLSState struct {
	baseTLSConfig *tls.Config
	sessionCaches *outbound.SessionCacheManager
}

func newInboundTLSReloader(
	role string,
	tlsConfig config.ServerTLS,
	authConfig config.ServerAuth,
	state *atomic.Pointer[inboundTLSState],
	logger zerolog.Logger,
) (*tlsreload.Reloader, error) {
	paths := tlsreload.Paths{CertFile: tlsConfig.ServerCertFile, KeyFile: tlsConfig.ServerKeyFile}
	if authConfig.Method == "" || authConfig.Method == config.ClientAuthMethodMTLS {
		paths.CAFile = authConfig.CACertFile
	}
	reloader, err := tlsreload.New(role, paths, logger, func(bundle *tlsreload.Bundle) error {
		if bundle.Certificate == nil {
			return fmt.Errorf("mesh server TLS certificate is unavailable")
		}
		state.Store(&inboundTLSState{certificate: *bundle.Certificate, clientCAs: bundle.CAPool})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := reloader.LoadInitial(); err != nil {
		reloader.Stop()
		return nil, err
	}
	return reloader, nil
}

func newOutboundTLSReloader(
	role string,
	tlsConfig config.ClientTLS,
	authConfig config.ClientAuth,
	state *atomic.Pointer[outboundTLSState],
	logger zerolog.Logger,
) (*tlsreload.Reloader, error) {
	paths := tlsreload.Paths{CAFile: tlsConfig.CACertFile}
	if authConfig.Method == "" || authConfig.Method == config.ClientAuthMethodMTLS {
		paths.CertFile = tlsConfig.ClientCertFile
		paths.KeyFile = tlsConfig.ClientKeyFile
	}
	reloader, err := tlsreload.New(role, paths, logger, func(bundle *tlsreload.Bundle) error {
		base := &tls.Config{
			RootCAs:    bundle.CAPool,
			MinVersion: tls.VersionTLS13,
			MaxVersion: tls.VersionTLS13,
			NextProtos: []string{meshALPN},
		}
		if bundle.Certificate != nil {
			base.Certificates = []tls.Certificate{*bundle.Certificate}
		}
		state.Store(&outboundTLSState{
			baseTLSConfig: base,
			sessionCaches: outbound.NewSessionCacheManager(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := reloader.LoadInitial(); err != nil {
		reloader.Stop()
		return nil, err
	}
	return reloader, nil
}
