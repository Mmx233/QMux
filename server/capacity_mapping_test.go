package server

import (
	"testing"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/server/pool"
)

func TestPoolLimitsFromCapacity(t *testing.T) {
	capacity := config.ListenerCapacity{
		MaxClientGenerations:             11,
		MaxPendingRegistrations:          22,
		MaxTCPConnectionsPerGeneration:   33,
		MaxPendingTCPSetupsPerGeneration: 44,
		MaxUDPSessionsPerGeneration:      55,
	}
	want := pool.Limits{
		MaxClientGenerations:             11,
		MaxPendingRegistrations:          22,
		MaxTCPConnectionsPerGeneration:   33,
		MaxPendingTCPSetupsPerGeneration: 44,
		MaxUDPSessionsPerGeneration:      55,
	}
	if got := poolLimitsFromCapacity(capacity); got != want {
		t.Fatalf("poolLimitsFromCapacity() = %+v, want %+v", got, want)
	}
}

func TestPoolLimitsFromDefaultedCapacity(t *testing.T) {
	var capacity config.ListenerCapacity
	capacity.ApplyDefaults()
	want := pool.Limits{
		MaxClientGenerations:             config.DefaultMaxClientGenerations,
		MaxPendingRegistrations:          config.DefaultMaxPendingRegistrations,
		MaxTCPConnectionsPerGeneration:   config.DefaultMaxTCPConnectionsPerGeneration,
		MaxPendingTCPSetupsPerGeneration: config.DefaultMaxPendingTCPSetupsPerGeneration,
		MaxUDPSessionsPerGeneration:      config.DefaultMaxUDPSessionsPerGeneration,
	}
	if got := poolLimitsFromCapacity(capacity); got != want {
		t.Fatalf("default pool limits = %+v, want %+v", got, want)
	}
}
