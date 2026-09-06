package run

import (
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/internal/stats"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server"
	"github.com/Mmx233/QMux/server/pool"
	"github.com/prometheus/client_golang/prometheus"
)

// Descriptors belong to one admin instance; collecting never registers metrics
// or updates application counters. Every scrape reads its source snapshot once.
type snapshotCollector struct {
	role    string
	descs   map[string]*prometheus.Desc
	collect func(chan<- prometheus.Metric)
}

func (c *snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		ch <- desc
	}
}

func (c *snapshotCollector) Collect(ch chan<- prometheus.Metric) { c.collect(ch) }

func (c *snapshotCollector) define(name, help string, labels ...string) {
	c.descs[name] = prometheus.NewDesc(prometheus.BuildFQName("qmux", c.role, name), help, labels, nil)
}

func (c *snapshotCollector) gauge(ch chan<- prometheus.Metric, name string, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.GaugeValue, value, labels...)
}

func (c *snapshotCollector) counter(ch chan<- prometheus.Metric, name string, value uint64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.CounterValue, float64(value), labels...)
}

func number(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func timestamp(value time.Time) float64 {
	if value.IsZero() {
		return 0
	}
	return float64(value.UnixNano()) / 1e9
}

func newSnapshotCollector(role string) *snapshotCollector {
	c := &snapshotCollector{role: role, descs: make(map[string]*prometheus.Desc)}
	c.define("ready", "Whether the running instance is ready (1) or not ready (0).")
	return c
}

func (c *snapshotCollector) defineTransport(scope string) {
	for name, help := range map[string]string{
		"quic_sent_bytes_total":          "QUIC bytes sent, including retransmissions; excludes outer UDP/IP framing.",
		"quic_received_bytes_total":      "QUIC bytes received, including duplicates; excludes outer UDP/IP framing.",
		"quic_sent_packets_total":        "QUIC packets sent, including packets later declared lost.",
		"quic_received_packets_total":    "QUIC packets received, including unprocessable packets.",
		"quic_lost_bytes":                "QUIC bytes currently estimated lost, including retired connections; can decrease after late ACKs.",
		"quic_lost_packets":              "QUIC packets currently estimated lost, including retired connections; can decrease after late ACKs.",
		"quic_connections":               "Open tracked QUIC connections contributing to current RTT statistics.",
		"quic_smoothed_rtt_seconds":      "Mean of the current connections' smoothed RTT estimates; zero without connections.",
		"quic_smoothed_rtt_max_seconds":  "Largest current connection smoothed RTT estimate; zero without connections.",
		"quic_rtt_deviation_max_seconds": "Largest current connection RTT mean-deviation estimate; zero without connections.",
	} {
		c.define(name, help, scope)
	}
}

func (c *snapshotCollector) transport(ch chan<- prometheus.Metric, s stats.TransportSnapshot, scope string) {
	c.counter(ch, "quic_sent_bytes_total", s.SentBytes, scope)
	c.counter(ch, "quic_received_bytes_total", s.ReceivedBytes, scope)
	c.counter(ch, "quic_sent_packets_total", s.SentPackets, scope)
	c.counter(ch, "quic_received_packets_total", s.ReceivedPackets, scope)
	c.gauge(ch, "quic_lost_bytes", float64(s.LostBytes), scope)
	c.gauge(ch, "quic_lost_packets", float64(s.LostPackets), scope)
	c.gauge(ch, "quic_connections", float64(s.Connections), scope)
	var mean float64
	if s.Connections > 0 {
		mean = s.SmoothedRTTSum.Seconds() / float64(s.Connections)
	}
	c.gauge(ch, "quic_smoothed_rtt_seconds", mean, scope)
	c.gauge(ch, "quic_smoothed_rtt_max_seconds", s.SmoothedRTTMax.Seconds(), scope)
	c.gauge(ch, "quic_rtt_deviation_max_seconds", s.RTTDeviationMax.Seconds(), scope)
}

func (c *snapshotCollector) defineOperation(name, help string, scope ...string) {
	c.define(name+"_attempts_total", help+" attempts started.", scope...)
	c.define(name+"_results_total", help+" attempts completed, partitioned by bounded result.", append(scope, "result")...)
	c.define(name+"_duration_seconds", help+" duration of completed attempts in seconds.", scope...)
}

func (c *snapshotCollector) operation(ch chan<- prometheus.Metric, name string, s stats.OperationSnapshot, scope ...string) {
	c.counter(ch, name+"_attempts_total", s.Attempts, scope...)
	for _, result := range []string{"success", "error", "timeout", "canceled", "dial_error", "auth_error", "protocol_error", "capacity", "ack_error"} {
		labels := append(append([]string(nil), scope...), result)
		c.counter(ch, name+"_results_total", s.Results[result], labels...)
	}
	buckets := s.Duration.Buckets
	if buckets == nil {
		buckets = make(map[float64]uint64, len(prometheus.DefBuckets))
		for _, bound := range prometheus.DefBuckets {
			buckets[bound] = 0
		}
	}
	ch <- prometheus.MustNewConstHistogram(c.descs[name+"_duration_seconds"], s.Duration.Count, s.Duration.Sum, buckets, scope...)
}

func (c *snapshotCollector) defineFragments(scope ...string) {
	c.define("udp_fragment_groups", "Retained or being-reserved fragment groups.", scope...)
	c.define("udp_fragment_backing_bytes", "Fragment backing memory held or being reserved in bytes.", scope...)
	c.define("udp_fragment_drops_total", "Fragment reassembly capacity rejections or expired incomplete groups, by reason.", append(scope, "reason")...)
}

func (c *snapshotCollector) fragments(ch chan<- prometheus.Metric, s protocol.FragmentSnapshot, scope ...string) {
	c.gauge(ch, "udp_fragment_groups", float64(s.RetainedGroups), scope...)
	c.gauge(ch, "udp_fragment_backing_bytes", float64(s.RetainedBackingBytes), scope...)
	for reason, count := range map[string]uint64{"group_capacity": s.GroupCapacityDrops, "byte_capacity": s.ByteCapacityDrops, "expired": s.ExpiredGroups} {
		c.counter(ch, "udp_fragment_drops_total", count, append(append([]string(nil), scope...), reason)...)
	}
}

func (c *snapshotCollector) defineUDP(scope ...string) {
	for name, help := range map[string]string{
		"udp_sessions":                      "Published local UDP sessions.",
		"udp_session_permits":               "UDP session capacity permits held, including sessions being created or removed.",
		"udp_sessions_limit":                "Configured UDP session capacity.",
		"udp_sessions_high_water":           "Lifetime peak held UDP session permits.",
		"udp_send_items":                    "Application datagrams owned by senders, including sends in progress; not the QUIC internal queue.",
		"udp_send_backing_bytes":            "Backing memory owned by application datagram sends in bytes, not traffic volume.",
		"udp_send_items_high_water":         "High-water application datagram ownership; server peaks are scrape-sampled.",
		"udp_send_backing_high_water_bytes": "High-water datagram backing memory; server peaks are scrape-sampled.",
		"udp_send_workers":                  "Current application UDP sender workers.",
		"udp_send_errors_total":             "Failed application QUIC datagram send operations.",
		"udp_accounting_faults_total":       "Internal UDP session ownership accounting faults.",
	} {
		c.define(name, help, scope...)
	}
	c.define("udp_drops_total", "UDP admission or processing rejections, partitioned by reason; not an end-to-end packet loss count.", append(scope, "reason")...)
}

func newServerCollector(snapshot func() server.Snapshot) prometheus.Collector {
	c := newSnapshotCollector("server")
	c.defineTransport("listener")
	c.defineOperation("registration", "Post-accept client registration", "listener")
	c.defineFragments("listener")
	c.defineUDP("listener")
	for name, help := range map[string]string{
		"route_ready":                                 "Whether this configured traffic route is ready.",
		"route_listening":                             "Whether the traffic manager is running for this configured route.",
		"quic_handshakes":                             "Pre-accept QUIC handshakes currently owned.",
		"quic_handshakes_high_water":                  "Lifetime peak concurrent pre-accept QUIC handshakes.",
		"quic_handshake_accounting_faults_total":      "Internal handshake ownership accounting faults.",
		"pending_registrations":                       "Accepted connections whose registration has not committed.",
		"healthy_clients":                             "Healthy currently registered client generations, irrespective of protocol capability.",
		"oldest_heartbeat_received_timestamp_seconds": "Oldest last received heartbeat among current registered generations; zero if none has received one.",
		"pool_accounting_faults_total":                "Internal connection pool accounting faults.",
		"tcp_flow_connections":                        "Total admitted TCP flows, including setup and active relay ownership.",
		"tcp_flow_connections_limit":                  "Listener TCP flow capacity.",
		"tcp_flow_connections_high_water":             "Lifetime peak listener TCP flow ownership.",
		"tcp_setup_connections":                       "TCP connections whose setup has not finished.",
		"tcp_setup_connections_limit":                 "Listener TCP setup capacity.",
		"tcp_setup_connections_high_water":            "Lifetime peak concurrent listener TCP setups.",
		"tcp_active_connections":                      "Currently active listener TCP relays.",
		"tcp_active_connections_high_water":           "Lifetime peak active listener TCP relays.",
		"tcp_setup_attempts_total":                    "TCP generation setup attempts, including retries on other generations.",
		"tcp_setup_retries_total":                     "TCP setup attempts after the first generation attempt.",
	} {
		c.define(name, help, "listener")
	}
	c.define("route_info", "Configured traffic route metadata.", "listener", "traffic_address", "protocol")
	c.define("eligible_clients", "Healthy registered generations supporting the requested protocol; does not imply spare capacity.", "listener", "protocol")
	c.define("client_generations", "Current client generations by disjoint ownership state.", "listener", "state")
	c.define("pool_tcp_connections", "TCP leases on current and retiring client generations.", "listener", "state")
	c.define("pool_udp_sessions", "UDP leases on current and retiring client generations.", "listener")
	c.define("capacity_usage", "Capacity usage; per-generation resources report the largest single-generation occupancy, not a sum.", "listener", "resource")
	c.define("capacity_limit", "Configured capacity; per-generation resources apply independently to each generation.", "listener", "resource")
	c.define("capacity_high_water", "Lifetime peak capacity occupancy; per-generation resources report the largest single-generation peak.", "listener", "resource")
	c.define("capacity_drops_total", "Capacity gate rejections by resource; a rejected generation can still be followed by a successful retry.", "listener", "resource")
	c.define("tcp_setup_attempt_failures_total", "Individual TCP generation attempt failures, including failures followed by successful retry.", "listener", "reason")
	c.define("tcp_admissions_total", "Terminal TCP admission outcomes, one per accepted public TCP connection.", "listener", "result")
	c.collect = func(ch chan<- prometheus.Metric) {
		s := snapshot()
		c.gauge(ch, "ready", number(s.Ready))
		for _, r := range s.Routes {
			l := r.QuicAddr
			c.gauge(ch, "route_info", 1, l, r.TrafficAddr, r.Protocol)
			c.gauge(ch, "route_ready", number(r.Ready), l)
			c.gauge(ch, "route_listening", number(r.Listening), l)
			c.gauge(ch, "eligible_clients", float64(r.TCPEligibleClients), l, "tcp")
			c.gauge(ch, "eligible_clients", float64(r.UDPEligibleClients), l, "udp")
			c.gauge(ch, "quic_handshakes", float64(r.Handshake.Current), l)
			c.gauge(ch, "quic_handshakes_high_water", float64(r.Handshake.HighWater), l)
			c.counter(ch, "quic_handshake_accounting_faults_total", r.Handshake.AccountingFaults, l)
			p := r.PoolCapacity
			c.transport(ch, p.QUIC, l)
			c.operation(ch, "registration", p.Registrations, l)
			c.gauge(ch, "healthy_clients", float64(p.Healthy), l)
			c.gauge(ch, "oldest_heartbeat_received_timestamp_seconds", timestamp(p.OldestHeartbeat), l)
			c.gauge(ch, "pending_registrations", float64(p.ServerPending), l)
			for state, count := range map[string]int{"reserved": p.Reservations, "registered": p.Registered, "retiring": p.ServerRetiring} {
				c.gauge(ch, "client_generations", float64(count), l, state)
			}
			c.gauge(ch, "pool_tcp_connections", float64(p.TCPPending), l, "setup")
			c.gauge(ch, "pool_tcp_connections", float64(p.TCPActive), l, "active")
			c.gauge(ch, "pool_udp_sessions", float64(p.UDPSessions), l)
			c.counter(ch, "pool_accounting_faults_total", p.AccountingFaults, l)
			for resource, limit := range map[string]pool.LimitSnapshot{
				"pending_registrations": p.PendingRegistrations, "client_generations": p.ClientGenerations,
				"tcp_connections_per_generation":    p.TCPConnectionsPerGeneration,
				"pending_tcp_setups_per_generation": p.PendingTCPSetupsPerGeneration,
				"udp_sessions_per_generation":       p.UDPSessionsPerGeneration,
			} {
				c.gauge(ch, "capacity_usage", float64(limit.Current), l, resource)
				c.gauge(ch, "capacity_limit", float64(limit.Limit), l, resource)
				c.gauge(ch, "capacity_high_water", float64(limit.HighWater), l, resource)
				c.counter(ch, "capacity_drops_total", limit.CapacityDrops, l, resource)
			}
			t := r.TCPAdmission
			for name, value := range map[string]int64{
				"tcp_flow_connections": t.FlowCurrent, "tcp_flow_connections_limit": t.FlowLimit, "tcp_flow_connections_high_water": t.FlowHighWater,
				"tcp_setup_connections": t.SetupCurrent, "tcp_setup_connections_limit": t.SetupLimit, "tcp_setup_connections_high_water": t.SetupHighWater,
				"tcp_active_connections": t.ActiveCurrent, "tcp_active_connections_high_water": t.ActiveHighWater,
			} {
				c.gauge(ch, name, float64(value), l)
			}
			c.counter(ch, "tcp_setup_attempts_total", t.Attempts, l)
			c.counter(ch, "tcp_setup_retries_total", t.Retries, l)
			c.counter(ch, "tcp_setup_attempt_failures_total", t.StreamLimitAttempts, l, "stream_limit")
			c.counter(ch, "tcp_setup_attempt_failures_total", t.AckFailureAttempts, l, "ack_failure")
			for result, count := range map[string]uint64{
				"committed": t.Committed, "flow_capacity": t.FlowCapacity, "setup_capacity": t.ListenerCapacity,
				"unavailable": t.Unavailable, "generation_connection_capacity": t.GenerationConnectionCapacity,
				"generation_setup_capacity": t.GenerationSetupCapacity, "peer_stream_limit": t.PeerStreamLimit,
				"deadline": t.Deadline, "setup_failure": t.SetupFailure, "canceled": t.Canceled,
			} {
				c.counter(ch, "tcp_admissions_total", count, l, result)
			}
			u := r.UDPAdmission
			for name, value := range map[string]int64{
				"udp_sessions": u.SessionsCurrent, "udp_session_permits": u.SessionPermits, "udp_sessions_limit": u.SessionLimit,
				"udp_sessions_high_water": u.SessionHighWater, "udp_send_items": u.DSendItems, "udp_send_backing_bytes": u.DSendBackingBytes,
				"udp_send_items_high_water": u.DSendItemsHighWater, "udp_send_backing_high_water_bytes": u.DSendBackingBytesHighWater,
				"udp_send_workers": u.DSendWorkers,
			} {
				c.gauge(ch, name, float64(value), l)
			}
			c.counter(ch, "udp_send_errors_total", u.DSendErrors, l)
			c.counter(ch, "udp_accounting_faults_total", u.AccountingFaults, l)
			for reason, count := range map[string]uint64{
				"listener_capacity": u.ListenerCapacityDrops, "generation_capacity": u.GenerationCapacityDrops,
				"queue_full": u.QueueFullDrops, "no_eligible_client": u.NoEligibleDrops, "fragmentation": u.FragmentDrops,
				"decode": u.DecodeDrops, "unknown_session": u.UnknownSessionDrops, "public_write": u.PublicWriteDrops,
			} {
				c.counter(ch, "udp_drops_total", count, l, reason)
			}
			c.fragments(ch, u.Fragment, l)
		}
	}
	return c
}

func newClientCollector(snapshot func() client.Snapshot) prometheus.Collector {
	c := newSnapshotCollector("client")
	c.defineTransport("endpoint")
	c.defineOperation("connection", "QUIC connection establishment including address resolution", "endpoint")
	c.defineOperation("registration", "Client registration acknowledgment", "endpoint")
	c.defineOperation("tcp_setup", "Incoming TCP stream setup through backend dial and acknowledgment")
	c.defineOperation("tcp_dial", "Local TCP backend dial")
	c.defineFragments()
	c.defineUDP()
	for name, help := range map[string]string{
		"connection_generations_high_water":    "Lifetime peak simultaneous connection generations for the configured endpoint.",
		"connection_accounting_faults_total":   "Internal connection generation accounting faults.",
		"connection_healthy":                   "Whether this endpoint currently has a healthy registered connection.",
		"heartbeat_received_timestamp_seconds": "Last received server heartbeat Unix timestamp; zero before receiving a heartbeat.",
		"reconnecting":                         "Whether reconnection is scheduled or in progress, including backoff.",
		"reconnect_attempts_total":             "Actual reconnect attempts started after backoff, excluding initial connection attempts.",
	} {
		c.define(name, help, "endpoint")
	}
	c.define("connection_generations", "Current connection generations by disjoint lifecycle phase.", "endpoint", "state")
	c.define("tcp_setup_connections", "Incoming TCP streams whose setup has not finished.")
	c.define("tcp_active_connections", "TCP relays with a connected backend and successful setup acknowledgment.")
	c.define("udp_local_errors_total", "Local UDP session creation or socket I/O failures, excluding normal shutdown.", "operation")
	c.define("udp_fragment_assemblers", "Live client fragment assemblers, including retiring handlers not yet joined.")
	c.collect = func(ch chan<- prometheus.Metric) {
		s := snapshot()
		c.gauge(ch, "ready", number(s.Ready))
		for _, e := range s.Endpoints {
			c.transport(ch, e.QUIC, e.Endpoint)
			c.operation(ch, "connection", e.Connect, e.Endpoint)
			c.operation(ch, "registration", e.Registration, e.Endpoint)
			for state, count := range map[string]int64{"handshaking": e.Handshaking, "pending": e.Pending, "registered": e.Registered, "retiring": e.Retiring} {
				c.gauge(ch, "connection_generations", float64(count), e.Endpoint, state)
			}
			c.gauge(ch, "connection_generations_high_water", float64(e.GenerationHighWater), e.Endpoint)
			c.counter(ch, "connection_accounting_faults_total", e.AccountingFaults, e.Endpoint)
			c.gauge(ch, "connection_healthy", number(e.Healthy), e.Endpoint)
			c.gauge(ch, "heartbeat_received_timestamp_seconds", timestamp(e.LastHeartbeat), e.Endpoint)
			c.gauge(ch, "reconnecting", number(e.Reconnecting), e.Endpoint)
			c.counter(ch, "reconnect_attempts_total", e.ReconnectAttempts, e.Endpoint)
		}
		c.gauge(ch, "tcp_setup_connections", float64(s.TCP.Pending))
		c.gauge(ch, "tcp_active_connections", float64(s.TCP.Active))
		c.operation(ch, "tcp_setup", s.TCP.Setups)
		c.operation(ch, "tcp_dial", s.TCP.Dials)
		u, d := s.UDPSessions, s.DSend
		for name, value := range map[string]int64{
			"udp_sessions": u.Current, "udp_session_permits": u.Permits, "udp_sessions_limit": u.Limit,
			"udp_sessions_high_water": u.HighWater, "udp_send_items": d.OwnedItems, "udp_send_backing_bytes": d.OwnedBacking,
			"udp_send_items_high_water": d.OwnedItemsHighWater, "udp_send_backing_high_water_bytes": d.OwnedBackingHighWater,
			"udp_send_workers": d.Workers,
		} {
			c.gauge(ch, name, float64(value))
		}
		c.counter(ch, "udp_send_errors_total", d.SendErrors)
		c.counter(ch, "udp_accounting_faults_total", u.AccountingFaults)
		c.counter(ch, "udp_drops_total", u.CapacityDrops, "session_capacity")
		c.counter(ch, "udp_drops_total", u.DecodeDrops, "decode")
		c.counter(ch, "udp_drops_total", d.FragmentDrops, "fragmentation")
		c.counter(ch, "udp_local_errors_total", u.CreateErrors, "create")
		c.counter(ch, "udp_local_errors_total", u.ReadErrors, "read")
		c.counter(ch, "udp_local_errors_total", u.WriteErrors, "write")
		c.gauge(ch, "udp_fragment_assemblers", float64(s.LiveAssemblers))
		c.fragments(ch, s.Fragments)
	}
	return c
}
