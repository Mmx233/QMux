package config

import (
	"bytes"
	"cmp"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	MeshRoutingPolicyShortestPathFirst = "shortest_path_first"
	MeshRoutingPolicyProbeAll          = "probe_all"

	MeshOutdatedClientPolicyApplyLatestRules = "apply_latest_rules"
	MeshOutdatedClientPolicyPauseNewTraffic  = "pause_new_traffic"

	MeshProbeTypeTCP  = "tcp"
	MeshProbeTypeHTTP = "http"

	MeshIngressProtocolHTTP           = "http"
	MeshIngressProtocolHTTPS          = "https"
	MeshIngressProtocolTLSPassthrough = "tls_passthrough"

	MeshPathMatchExact      = "Exact"
	MeshPathMatchPathPrefix = "PathPrefix"
	MeshValueMatchExact     = "Exact"

	defaultMeshProbeInterval         = 10 * time.Second
	defaultMeshProbeTimeout          = 3 * time.Second
	defaultMeshProbeFailureThreshold = 3
	defaultMeshProbeSuccessThreshold = 2
	defaultMeshProbeMaxConcurrent    = 64
	defaultMeshProbeMaxQueued        = 1024

	defaultMeshMaxPeers                      = 32
	defaultMeshMaxGroups                     = 1024
	defaultMeshMaxGroupDeclarationBytes      = 1 << 20
	defaultMeshMaxTotalGroupDeclarationBytes = 64 << 20
	defaultMeshMaxPathsPerGroup              = 256
	defaultMeshMaxTotalPaths                 = 16384
	defaultMeshMaxPathHops                   = 16
	defaultMeshMaxControlQueueMessages       = 1024
	defaultMeshMaxControlQueueBytes          = 16 << 20

	maxMeshHTTPRoutes       = 16
	maxMeshRouteHostnames   = 16
	maxMeshHTTPMatches      = 64
	maxMeshHTTPMatchHeaders = 16
	maxMeshHTTPMatchQueries = 16
	maxMeshHostnameLength   = 253
	maxMeshHTTPPathLength   = 1024
	maxMeshHTTPNameLength   = 256
	maxMeshHeaderValue      = 4096
	maxMeshQueryValue       = 1024
)

var (
	meshDNS1123SubdomainPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	meshHostnamePattern         = regexp.MustCompile(`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	meshHTTPNamePattern         = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+\\-.^_`|~]+$")
	meshHeaderValuePattern      = regexp.MustCompile("^[!-~]+([\\t ]+[!-~]+)*$")
)

type MeshServer struct {
	ServerID       string             `yaml:"server_id"`
	AdminAddress   string             `yaml:"admin_address"`
	Tunnel         MeshServerTunnel   `yaml:"tunnel"`
	Ingress        MeshIngress        `yaml:"ingress"`
	LoadBalancer   string             `yaml:"load_balancer"`
	RoutingPolicy  string             `yaml:"routing_policy"`
	ProbeScheduler MeshProbeScheduler `yaml:"probe_scheduler"`
	Limits         MeshServerLimits   `yaml:"limits"`
}

type MeshServerTunnel struct {
	Listen            MeshTunnelListen `yaml:"listen"`
	Peering           MeshPeering      `yaml:"peering"`
	Quic              Quic             `yaml:"quic"`
	Capacity          MeshTCPCapacity  `yaml:"capacity"`
	HeartbeatInterval time.Duration    `yaml:"heartbeat_interval"`
	HealthTimeout     time.Duration    `yaml:"health_timeout"`
	TCPCopyBufferSize int              `yaml:"tcp_copy_buffer_size"`
}

type MeshTunnelListen struct {
	Address string     `yaml:"address"`
	Auth    ServerAuth `yaml:"auth"`
	TLS     ServerTLS  `yaml:"tls"`
}

type MeshPeering struct {
	Peers []MeshPeer `yaml:"peers"`
	Auth  ClientAuth `yaml:"auth"`
	TLS   ClientTLS  `yaml:"tls"`
}

type MeshPeer struct {
	ServerID   string `yaml:"server_id"`
	Address    string `yaml:"address"`
	ServerName string `yaml:"server_name"`
	Metric     int64  `yaml:"metric"`
}

type MeshIngress struct {
	Listeners  []MeshIngressListener `yaml:"listeners"`
	ErrorPages MeshErrorPages        `yaml:"error_pages"`
}

type MeshIngressListener struct {
	Address             string                   `yaml:"address"`
	Protocol            string                   `yaml:"protocol"`
	Certificates        []MeshIngressCertificate `yaml:"certificates"`
	Capacity            MeshTCPCapacity          `yaml:"capacity"`
	MaxInflightRequests *int                     `yaml:"max_inflight_requests"`
}

type MeshIngressCertificate struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type MeshErrorPages struct {
	NotFoundFile           string `yaml:"not_found_file"`
	ServiceUnavailableFile string `yaml:"service_unavailable_file"`
}

type MeshProbeScheduler struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailureThreshold int           `yaml:"failure_threshold"`
	SuccessThreshold int           `yaml:"success_threshold"`
	MaxConcurrent    int           `yaml:"max_concurrent"`
	MaxQueued        int           `yaml:"max_queued"`
}

// MeshTCPCapacity keeps mesh TCP sources independent without exposing ordinary
// L4 registration or UDP capacity fields.
type MeshTCPCapacity struct {
	MaxTCPConnections                int `yaml:"max_tcp_connections"`
	MaxPendingTCPSetups              int `yaml:"max_pending_tcp_setups"`
	MaxTCPConnectionsPerGeneration   int `yaml:"max_tcp_connections_per_generation"`
	MaxPendingTCPSetupsPerGeneration int `yaml:"max_pending_tcp_setups_per_generation"`
}

// MeshServerLimits is intentionally separate from ListenerCapacity so mesh
// configuration cannot accept ordinary L4 UDP capacity fields.
type MeshServerLimits struct {
	MaxClientGenerations          int   `yaml:"max_client_generations"`
	MaxPendingRegistrations       int   `yaml:"max_pending_registrations"`
	MaxPeers                      int   `yaml:"max_peers"`
	MaxGroups                     int   `yaml:"max_groups"`
	MaxGroupDeclarationBytes      int64 `yaml:"max_group_declaration_bytes"`
	MaxTotalGroupDeclarationBytes int64 `yaml:"max_total_group_declaration_bytes"`
	MaxPathsPerGroup              int   `yaml:"max_paths_per_group"`
	MaxTotalPaths                 int   `yaml:"max_total_paths"`
	MaxPathHops                   int   `yaml:"max_path_hops"`
	MaxControlQueueMessages       int   `yaml:"max_control_queue_messages"`
	MaxControlQueueBytes          int64 `yaml:"max_control_queue_bytes"`
}

type MeshClient struct {
	InstanceID   string           `yaml:"instance_id"`
	AdminAddress string           `yaml:"admin_address"`
	Tunnel       MeshClientTunnel `yaml:"tunnel"`
	Local        LocalService     `yaml:"local"`
	Group        MeshGroup        `yaml:"group"`
}

type MeshClientTunnel struct {
	Servers           []MeshServerEndpoint `yaml:"servers"`
	Quic              Quic                 `yaml:"quic"`
	Auth              ClientAuth           `yaml:"auth"`
	TLS               ClientTLS            `yaml:"tls"`
	HeartbeatInterval time.Duration        `yaml:"heartbeat_interval"`
	HealthTimeout     time.Duration        `yaml:"health_timeout"`
	TCPCopyBufferSize int                  `yaml:"tcp_copy_buffer_size"`
}

type MeshServerEndpoint struct {
	ServerID   string `yaml:"server_id"`
	Address    string `yaml:"address"`
	ServerName string `yaml:"server_name"`
}

type MeshGroup struct {
	GroupID              string        `yaml:"group_id"`
	RuleVersion          uint64        `yaml:"rule_version"`
	Metric               int64         `yaml:"metric"`
	OutdatedClientPolicy string        `yaml:"outdated_client_policy"`
	Routes               MeshRoutes    `yaml:"routes"`
	Probe                MeshProbe     `yaml:"probe"`
	OriginTLS            MeshOriginTLS `yaml:"origin_tls"`
	canonicalBytes       []byte
}

type MeshRoutes struct {
	HTTP           []MeshHTTPRoute `yaml:"http"`
	TLSPassthrough *MeshTLSRoute   `yaml:"tls_passthrough"`
}

type MeshHTTPRoute struct {
	Hostnames []string        `yaml:"hostnames"`
	Matches   []MeshHTTPMatch `yaml:"matches"`
}

type MeshTLSRoute struct {
	Hostnames []string `yaml:"hostnames"`
}

type MeshHTTPMatch struct {
	Path        *MeshHTTPPathMatch        `yaml:"path"`
	Method      string                    `yaml:"method"`
	Headers     []MeshHTTPHeaderMatch     `yaml:"headers"`
	QueryParams []MeshHTTPQueryParamMatch `yaml:"query_params"`
}

type MeshHTTPPathMatch struct {
	Type  string `yaml:"type"`
	Value string `yaml:"value"`
}

type MeshHTTPHeaderMatch struct {
	Type  string `yaml:"type"`
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type MeshHTTPQueryParamMatch struct {
	Type  string `yaml:"type"`
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type MeshProbe struct {
	Type string `yaml:"type"`
	Host string `yaml:"host"`
	Path string `yaml:"path"`
}

type MeshOriginTLS struct {
	Enabled             bool     `yaml:"enabled"`
	Verify              bool     `yaml:"verify"`
	ExtraCAFiles        []string `yaml:"extra_ca_files"`
	ExtraCACertificates [][]byte `yaml:"-"`
}

func LoadMeshServerConfig(path string) (*MeshServer, error) {
	cfg, err := loadMeshConfig[MeshServer](path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("mesh server configuration validation failed: %w", err)
	}
	return cfg, nil
}

func LoadMeshClientConfig(path string) (*MeshClient, error) {
	cfg, err := loadMeshConfig[MeshClient](path)
	if err != nil {
		return nil, err
	}
	if err := FinalizeMeshClientConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// FinalizeMeshClientConfig prepares the same public declaration for loaded and
// typed configurations. It does not trust a cached canonical projection.
func FinalizeMeshClientConfig(cfg *MeshClient) error {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("mesh client configuration validation failed: %w", err)
	}
	cfg.Group.normalize()
	certificates, err := loadMeshExtraCACertificates(cfg.Group.OriginTLS.ExtraCAFiles)
	if err != nil {
		return fmt.Errorf("mesh client configuration validation failed: %w", err)
	}
	cfg.Group.OriginTLS.ExtraCACertificates = certificates
	cfg.Group.canonicalBytes, err = marshalMeshGroupCanonical(cfg.Group)
	if err != nil {
		return fmt.Errorf("marshal mesh group canonical declaration: %w", err)
	}
	return nil
}

// CloneMeshClientConfig isolates mutable nested values before defaults and
// normalization are applied to a typed configuration.
func CloneMeshClientConfig(cfg *MeshClient) MeshClient {
	owned := *cfg
	owned.Tunnel.Servers = slices.Clone(cfg.Tunnel.Servers)
	g := &owned.Group
	g.Routes.HTTP = slices.Clone(cfg.Group.Routes.HTTP)
	for i := range g.Routes.HTTP {
		route := &g.Routes.HTTP[i]
		route.Hostnames = slices.Clone(route.Hostnames)
		route.Matches = slices.Clone(route.Matches)
		for j := range route.Matches {
			match := &route.Matches[j]
			if match.Path != nil {
				path := *match.Path
				match.Path = &path
			}
			match.Headers = slices.Clone(match.Headers)
			match.QueryParams = slices.Clone(match.QueryParams)
		}
	}
	if g.Routes.TLSPassthrough != nil {
		route := *g.Routes.TLSPassthrough
		route.Hostnames = slices.Clone(route.Hostnames)
		g.Routes.TLSPassthrough = &route
	}
	g.OriginTLS.ExtraCAFiles = slices.Clone(cfg.Group.OriginTLS.ExtraCAFiles)
	g.OriginTLS.ExtraCACertificates = slices.Clone(cfg.Group.OriginTLS.ExtraCACertificates)
	for i, certificate := range cfg.Group.OriginTLS.ExtraCACertificates {
		g.OriginTLS.ExtraCACertificates[i] = bytes.Clone(certificate)
	}
	g.canonicalBytes = nil
	return owned
}

func loadMeshConfig[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := validateMeshIntegerData(data, reflect.TypeFor[T]()); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return decodeConfig[T](data)
}

type meshYAMLIntegerNode struct {
	node yaml.Node
}

func (n *meshYAMLIntegerNode) UnmarshalYAML(node *yaml.Node) error {
	n.node = *node
	return nil
}

func validateMeshIntegerData(data []byte, target reflect.Type) error {
	shadow := reflect.New(meshYAMLShadowType(target))
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(shadow.Interface()); err != nil && err != io.EOF {
		return err
	}
	if err := validateMeshIntegerValues(shadow.Elem(), target, ""); err != nil {
		return err
	}
	if target == reflect.TypeFor[MeshServer]() {
		return validateMeshInflightNull(data)
	}
	return nil
}

func validateMeshInflightNull(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	field := func(node *yaml.Node, name string) *yaml.Node {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == name {
				return node.Content[i+1]
			}
		}
		return nil
	}
	listeners := field(field(document.Content[0], "ingress"), "listeners")
	if listeners == nil || listeners.Kind != yaml.SequenceNode {
		return nil
	}
	for i, listener := range listeners.Content {
		limit := field(listener, "max_inflight_requests")
		if limit != nil && limit.ShortTag() == "!!null" {
			return fmt.Errorf("ingress.listeners[%d].max_inflight_requests must be an integer", i)
		}
	}
	return nil
}

func meshYAMLShadowType(target reflect.Type) reflect.Type {
	if target == reflect.TypeFor[time.Duration]() {
		return reflect.TypeFor[yaml.Node]()
	}
	switch target.Kind() {
	case reflect.Pointer:
		return reflect.PointerTo(meshYAMLShadowType(target.Elem()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return reflect.TypeFor[meshYAMLIntegerNode]()
	case reflect.Struct:
		fields := make([]reflect.StructField, 0, target.NumField())
		for field := range target.Fields() {
			if _, _, ok := meshYAMLField(field); !ok {
				continue
			}
			fields = append(fields, reflect.StructField{
				Name: field.Name,
				Type: meshYAMLShadowType(field.Type),
				Tag:  field.Tag,
			})
		}
		return reflect.StructOf(fields)
	case reflect.Slice:
		return reflect.SliceOf(meshYAMLShadowType(target.Elem()))
	case reflect.Array:
		return reflect.ArrayOf(target.Len(), meshYAMLShadowType(target.Elem()))
	default:
		return reflect.TypeFor[yaml.Node]()
	}
}

func validateMeshIntegerValues(shadow reflect.Value, target reflect.Type, path string) error {
	for target.Kind() == reflect.Pointer {
		if shadow.IsNil() {
			return nil
		}
		shadow = shadow.Elem()
		target = target.Elem()
	}
	if target == reflect.TypeFor[time.Duration]() {
		return nil
	}

	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		raw := shadow.Interface().(meshYAMLIntegerNode)
		if raw.node.Kind == 0 {
			return nil
		}
		return validateMeshIntegerNode(&raw.node, target, path, false)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		raw := shadow.Interface().(meshYAMLIntegerNode)
		if raw.node.Kind == 0 {
			return nil
		}
		return validateMeshIntegerNode(&raw.node, target, path, true)
	case reflect.Struct:
		for field := range target.Fields() {
			name, inline, ok := meshYAMLField(field)
			if !ok {
				continue
			}
			fieldPath := path
			if !inline {
				fieldPath = name
				if path != "" {
					fieldPath = path + "." + name
				}
			}
			if err := validateMeshIntegerValues(shadow.FieldByName(field.Name), field.Type, fieldPath); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < shadow.Len(); i++ {
			if err := validateMeshIntegerValues(shadow.Index(i), target.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMeshIntegerNode(node *yaml.Node, target reflect.Type, path string, unsigned bool) error {
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" {
		return fmt.Errorf("%s must be an integer", path)
	}
	value := strings.ReplaceAll(node.Value, "_", "")
	if unsigned {
		if strings.HasPrefix(value, "-") {
			return fmt.Errorf("%s must be an unsigned %d-bit integer", path, target.Bits())
		}
		value = strings.TrimPrefix(value, "+")
		if _, err := strconv.ParseUint(value, 0, target.Bits()); err != nil {
			return fmt.Errorf("%s must be an unsigned %d-bit integer", path, target.Bits())
		}
		return nil
	}
	if _, err := strconv.ParseInt(value, 0, target.Bits()); err != nil {
		return fmt.Errorf("%s must be a signed %d-bit integer", path, target.Bits())
	}
	return nil
}

func meshYAMLField(field reflect.StructField) (name string, inline, ok bool) {
	if field.PkgPath != "" {
		return "", false, false
	}
	name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
	if name == "-" {
		return "", false, false
	}
	inline = strings.Contains(","+options+",", ",inline,")
	if name == "" {
		name = strings.ToLower(field.Name)
	}
	return name, inline, true
}

func (s *MeshServer) ApplyDefaults() {
	if s.Tunnel.HeartbeatInterval == 0 {
		s.Tunnel.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if s.Tunnel.HealthTimeout == 0 {
		s.Tunnel.HealthTimeout = DefaultHealthTimeout
	}
	if s.Tunnel.TCPCopyBufferSize == 0 {
		s.Tunnel.TCPCopyBufferSize = DefaultTCPCopyBufferSize
	}
	if s.LoadBalancer == "" {
		s.LoadBalancer = DefaultLoadBalancer
	}
	if s.RoutingPolicy == "" {
		s.RoutingPolicy = MeshRoutingPolicyShortestPathFirst
	}
	if s.Tunnel.Peering.hasDialPeer() || s.Tunnel.Peering.hasOutboundConfig() {
		s.Tunnel.Peering.Auth.ApplyDefaults()
	}
	s.ProbeScheduler.ApplyDefaults()
	s.Tunnel.Capacity.ApplyDefaults()
	for i := range s.Ingress.Listeners {
		listener := &s.Ingress.Listeners[i]
		listener.Capacity.ApplyDefaults()
		if listener.Protocol != MeshIngressProtocolTLSPassthrough && listener.MaxInflightRequests == nil {
			value := 128
			listener.MaxInflightRequests = &value
		} else if listener.MaxInflightRequests != nil && *listener.MaxInflightRequests == 0 {
			value := 128
			listener.MaxInflightRequests = &value
		}
	}
	s.Limits.ApplyDefaults()
}

func (c *MeshClient) ApplyDefaults() {
	c.Tunnel.Auth.ApplyDefaults()
	if c.Tunnel.HeartbeatInterval == 0 {
		c.Tunnel.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.Tunnel.HealthTimeout == 0 {
		c.Tunnel.HealthTimeout = DefaultHealthTimeout
	}
	if c.Tunnel.TCPCopyBufferSize == 0 {
		c.Tunnel.TCPCopyBufferSize = DefaultTCPCopyBufferSize
	}
	c.Group.ApplyDefaults()
}

func (p *MeshProbeScheduler) ApplyDefaults() {
	p.Interval = cmp.Or(p.Interval, defaultMeshProbeInterval)
	p.Timeout = cmp.Or(p.Timeout, defaultMeshProbeTimeout)
	p.FailureThreshold = cmp.Or(p.FailureThreshold, defaultMeshProbeFailureThreshold)
	p.SuccessThreshold = cmp.Or(p.SuccessThreshold, defaultMeshProbeSuccessThreshold)
	p.MaxConcurrent = cmp.Or(p.MaxConcurrent, defaultMeshProbeMaxConcurrent)
	p.MaxQueued = cmp.Or(p.MaxQueued, defaultMeshProbeMaxQueued)
}

func (l *MeshServerLimits) ApplyDefaults() {
	l.MaxClientGenerations = cmp.Or(l.MaxClientGenerations, DefaultMaxClientGenerations)
	l.MaxPendingRegistrations = cmp.Or(l.MaxPendingRegistrations, DefaultMaxPendingRegistrations)
	l.MaxPeers = cmp.Or(l.MaxPeers, defaultMeshMaxPeers)
	l.MaxGroups = cmp.Or(l.MaxGroups, defaultMeshMaxGroups)
	l.MaxGroupDeclarationBytes = cmp.Or(l.MaxGroupDeclarationBytes, int64(defaultMeshMaxGroupDeclarationBytes))
	l.MaxTotalGroupDeclarationBytes = cmp.Or(l.MaxTotalGroupDeclarationBytes, int64(defaultMeshMaxTotalGroupDeclarationBytes))
	l.MaxPathsPerGroup = cmp.Or(l.MaxPathsPerGroup, defaultMeshMaxPathsPerGroup)
	l.MaxTotalPaths = cmp.Or(l.MaxTotalPaths, defaultMeshMaxTotalPaths)
	l.MaxPathHops = cmp.Or(l.MaxPathHops, defaultMeshMaxPathHops)
	l.MaxControlQueueMessages = cmp.Or(l.MaxControlQueueMessages, defaultMeshMaxControlQueueMessages)
	l.MaxControlQueueBytes = cmp.Or(l.MaxControlQueueBytes, int64(defaultMeshMaxControlQueueBytes))
}

func (c *MeshTCPCapacity) ApplyDefaults() {
	c.MaxTCPConnections = cmp.Or(c.MaxTCPConnections, DefaultMaxTCPConnections)
	c.MaxPendingTCPSetups = cmp.Or(c.MaxPendingTCPSetups, DefaultMaxPendingTCPSetups)
	c.MaxTCPConnectionsPerGeneration = cmp.Or(c.MaxTCPConnectionsPerGeneration, DefaultMaxTCPConnectionsPerGeneration)
	c.MaxPendingTCPSetupsPerGeneration = cmp.Or(c.MaxPendingTCPSetupsPerGeneration, DefaultMaxPendingTCPSetupsPerGeneration)
}

func (c *MeshTCPCapacity) Validate(path string) error {
	values := []struct {
		name  string
		value int
	}{
		{"max_tcp_connections", c.MaxTCPConnections},
		{"max_pending_tcp_setups", c.MaxPendingTCPSetups},
		{"max_tcp_connections_per_generation", c.MaxTCPConnectionsPerGeneration},
		{"max_pending_tcp_setups_per_generation", c.MaxPendingTCPSetupsPerGeneration},
	}
	for _, value := range values {
		if value.value < 0 {
			return fmt.Errorf("%s.%s must not be negative", path, value.name)
		}
	}
	return nil
}

func (g *MeshGroup) ApplyDefaults() {
	if g.OutdatedClientPolicy == "" {
		g.OutdatedClientPolicy = MeshOutdatedClientPolicyApplyLatestRules
	}
	if g.Probe.Type == "" {
		g.Probe.Type = MeshProbeTypeTCP
	}
	for routeIndex := range g.Routes.HTTP {
		route := &g.Routes.HTTP[routeIndex]
		if len(route.Matches) == 0 {
			route.Matches = []MeshHTTPMatch{{}}
		}
		for matchIndex := range route.Matches {
			match := &route.Matches[matchIndex]
			if match.Path == nil {
				match.Path = &MeshHTTPPathMatch{}
			}
			if match.Path.Type == "" {
				match.Path.Type = MeshPathMatchPathPrefix
			}
			if match.Path.Value == "" {
				match.Path.Value = "/"
			}
			for i := range match.Headers {
				if match.Headers[i].Type == "" {
					match.Headers[i].Type = MeshValueMatchExact
				}
			}
			for i := range match.QueryParams {
				if match.QueryParams[i].Type == "" {
					match.QueryParams[i].Type = MeshValueMatchExact
				}
			}
		}
	}
}

func (s *MeshServer) Validate() error {
	if err := validateMeshIdentity("server_id", s.ServerID); err != nil {
		return err
	}
	if s.AdminAddress != "" {
		if err := validateListenerAddress(s.AdminAddress); err != nil {
			return fmt.Errorf("admin_address: %w", err)
		}
	}
	if err := validateListenerAddress(s.Tunnel.Listen.Address); err != nil {
		return fmt.Errorf("tunnel.listen.address: %w", err)
	}
	if err := s.Tunnel.Listen.Auth.Validate(); err != nil {
		return fmt.Errorf("tunnel.listen.auth: %w", err)
	}
	if err := validateMeshServerTLS(s.Tunnel.Listen.TLS, "tunnel.listen.tls"); err != nil {
		return err
	}
	if err := s.Tunnel.Quic.Validate("tunnel.quic"); err != nil {
		return err
	}
	if err := s.Tunnel.Capacity.Validate("tunnel.capacity"); err != nil {
		return err
	}
	if err := validateMeshTunnelSettings(s.Tunnel.HeartbeatInterval, s.Tunnel.HealthTimeout, s.Tunnel.TCPCopyBufferSize, "tunnel"); err != nil {
		return err
	}
	if err := s.validatePeers(); err != nil {
		return err
	}
	if err := s.validateIngress(); err != nil {
		return err
	}
	switch s.LoadBalancer {
	case "least-connections", "round-robin":
	default:
		return fmt.Errorf("load_balancer must be least-connections or round-robin, got %q", s.LoadBalancer)
	}
	switch s.RoutingPolicy {
	case MeshRoutingPolicyShortestPathFirst, MeshRoutingPolicyProbeAll:
	default:
		return fmt.Errorf("routing_policy must be %s or %s, got %q", MeshRoutingPolicyShortestPathFirst, MeshRoutingPolicyProbeAll, s.RoutingPolicy)
	}
	if err := s.ProbeScheduler.Validate("probe_scheduler"); err != nil {
		return err
	}
	if err := s.Limits.Validate("limits"); err != nil {
		return err
	}
	if len(s.Tunnel.Peering.Peers) > s.Limits.MaxPeers {
		return fmt.Errorf("tunnel.peering.peers has %d entries, exceeds limits.max_peers %d", len(s.Tunnel.Peering.Peers), s.Limits.MaxPeers)
	}
	return s.validateSocketClaims()
}

func (c *MeshClient) Validate() error {
	if err := validateMeshIdentity("instance_id", c.InstanceID); err != nil {
		return err
	}
	if c.AdminAddress != "" {
		if err := validateListenerAddress(c.AdminAddress); err != nil {
			return fmt.Errorf("admin_address: %w", err)
		}
	}
	if len(c.Tunnel.Servers) == 0 {
		return errors.New("tunnel.servers must contain at least one endpoint")
	}
	seenIDs := make(map[string]int, len(c.Tunnel.Servers))
	seenAddresses := make(map[string]int, len(c.Tunnel.Servers))
	for i, endpoint := range c.Tunnel.Servers {
		path := fmt.Sprintf("tunnel.servers[%d]", i)
		if err := validateMeshIdentity(path+".server_id", endpoint.ServerID); err != nil {
			return err
		}
		if previous, ok := seenIDs[endpoint.ServerID]; ok {
			return fmt.Errorf("%s.server_id duplicates tunnel.servers[%d].server_id", path, previous)
		}
		seenIDs[endpoint.ServerID] = i
		if err := ValidateAddress(endpoint.Address); err != nil {
			return fmt.Errorf("%s.address: %w", path, err)
		}
		if previous, ok := seenAddresses[endpoint.Address]; ok {
			return fmt.Errorf("%s.address duplicates tunnel.servers[%d].address", path, previous)
		}
		seenAddresses[endpoint.Address] = i
	}
	if err := c.Tunnel.Quic.Validate("tunnel.quic"); err != nil {
		return err
	}
	if err := c.Tunnel.Auth.Validate(); err != nil {
		return fmt.Errorf("tunnel.auth: %w", err)
	}
	if err := c.Tunnel.TLS.Validate(c.Tunnel.Auth.Method); err != nil {
		return fmt.Errorf("tunnel.tls: %w", err)
	}
	if err := validateMeshTunnelSettings(c.Tunnel.HeartbeatInterval, c.Tunnel.HealthTimeout, c.Tunnel.TCPCopyBufferSize, "tunnel"); err != nil {
		return err
	}
	if strings.TrimSpace(c.Local.Host) == "" {
		return errors.New("local.host is required")
	}
	if c.Local.Port < 1 || c.Local.Port > 65535 {
		return fmt.Errorf("local.port must be between 1 and 65535, got %d", c.Local.Port)
	}
	return c.Group.Validate("group")
}

func (p *MeshProbeScheduler) Validate(path string) error {
	values := []struct {
		name  string
		value int64
	}{
		{"interval", int64(p.Interval)},
		{"timeout", int64(p.Timeout)},
		{"failure_threshold", int64(p.FailureThreshold)},
		{"success_threshold", int64(p.SuccessThreshold)},
		{"max_concurrent", int64(p.MaxConcurrent)},
		{"max_queued", int64(p.MaxQueued)},
	}
	for _, value := range values {
		if value.value <= 0 {
			return fmt.Errorf("%s.%s must be positive", path, value.name)
		}
	}
	return nil
}

func (l *MeshServerLimits) Validate(path string) error {
	values := []struct {
		name  string
		value int64
	}{
		{"max_client_generations", int64(l.MaxClientGenerations)},
		{"max_pending_registrations", int64(l.MaxPendingRegistrations)},
		{"max_peers", int64(l.MaxPeers)},
		{"max_groups", int64(l.MaxGroups)},
		{"max_group_declaration_bytes", l.MaxGroupDeclarationBytes},
		{"max_total_group_declaration_bytes", l.MaxTotalGroupDeclarationBytes},
		{"max_paths_per_group", int64(l.MaxPathsPerGroup)},
		{"max_total_paths", int64(l.MaxTotalPaths)},
		{"max_path_hops", int64(l.MaxPathHops)},
		{"max_control_queue_messages", int64(l.MaxControlQueueMessages)},
		{"max_control_queue_bytes", l.MaxControlQueueBytes},
	}
	for _, value := range values {
		if value.value <= 0 {
			return fmt.Errorf("%s.%s must be positive", path, value.name)
		}
	}
	if l.MaxGroupDeclarationBytes > l.MaxTotalGroupDeclarationBytes {
		return fmt.Errorf("%s.max_group_declaration_bytes must not exceed %s.max_total_group_declaration_bytes", path, path)
	}
	if l.MaxPathsPerGroup > l.MaxTotalPaths {
		return fmt.Errorf("%s.max_paths_per_group must not exceed %s.max_total_paths", path, path)
	}
	return nil
}

func (g *MeshGroup) Validate(path string) error {
	if err := validateMeshIdentity(path+".group_id", g.GroupID); err != nil {
		return err
	}
	if g.RuleVersion == 0 {
		return fmt.Errorf("%s.rule_version must be at least 1", path)
	}
	switch g.OutdatedClientPolicy {
	case MeshOutdatedClientPolicyApplyLatestRules, MeshOutdatedClientPolicyPauseNewTraffic:
	default:
		return fmt.Errorf("%s.outdated_client_policy must be %s or %s", path, MeshOutdatedClientPolicyApplyLatestRules, MeshOutdatedClientPolicyPauseNewTraffic)
	}
	if len(g.Routes.HTTP) > maxMeshHTTPRoutes {
		return fmt.Errorf("%s.routes.http must contain at most %d entries", path, maxMeshHTTPRoutes)
	}
	for i := range g.Routes.HTTP {
		if err := g.Routes.HTTP[i].validate(fmt.Sprintf("%s.routes.http[%d]", path, i)); err != nil {
			return err
		}
	}
	if g.Routes.TLSPassthrough != nil {
		tlsPath := path + ".routes.tls_passthrough.hostnames"
		if len(g.Routes.TLSPassthrough.Hostnames) == 0 {
			return fmt.Errorf("%s must contain at least one hostname", tlsPath)
		}
		if err := validateMeshHostnames(tlsPath, g.Routes.TLSPassthrough.Hostnames); err != nil {
			return err
		}
	}
	if err := g.Probe.validate(path + ".probe"); err != nil {
		return err
	}
	return g.OriginTLS.validate(path + ".origin_tls")
}

func (r *MeshHTTPRoute) validate(path string) error {
	if err := validateMeshHostnames(path+".hostnames", r.Hostnames); err != nil {
		return err
	}
	if len(r.Matches) > maxMeshHTTPMatches {
		return fmt.Errorf("%s.matches must contain at most %d entries", path, maxMeshHTTPMatches)
	}
	for i := range r.Matches {
		if err := r.Matches[i].validate(fmt.Sprintf("%s.matches[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func (m *MeshHTTPMatch) validate(path string) error {
	if m.Path != nil {
		matchType := cmp.Or(m.Path.Type, MeshPathMatchPathPrefix)
		if matchType != MeshPathMatchExact && matchType != MeshPathMatchPathPrefix {
			return fmt.Errorf("%s.path.type must be Exact or PathPrefix", path)
		}
		if err := validateMeshHTTPPath(cmp.Or(m.Path.Value, "/")); err != nil {
			return fmt.Errorf("%s.path.value: %w", path, err)
		}
	}
	if m.Method != "" && !validMeshHTTPMethod(m.Method) {
		return fmt.Errorf("%s.method has unsupported value %q", path, m.Method)
	}
	if len(m.Headers) > maxMeshHTTPMatchHeaders {
		return fmt.Errorf("%s.headers must contain at most %d entries", path, maxMeshHTTPMatchHeaders)
	}
	for i, header := range m.Headers {
		itemPath := fmt.Sprintf("%s.headers[%d]", path, i)
		if header.Type != "" && header.Type != MeshValueMatchExact {
			return fmt.Errorf("%s.type must be Exact", itemPath)
		}
		if err := validateMeshHTTPName(header.Name); err != nil {
			return fmt.Errorf("%s.name: %w", itemPath, err)
		}
		if len(header.Value) < 1 || len(header.Value) > maxMeshHeaderValue || !meshHeaderValuePattern.MatchString(header.Value) {
			return fmt.Errorf("%s.value must be 1..%d bytes of printable ASCII separated by spaces or tabs", itemPath, maxMeshHeaderValue)
		}
	}
	if len(m.QueryParams) > maxMeshHTTPMatchQueries {
		return fmt.Errorf("%s.query_params must contain at most %d entries", path, maxMeshHTTPMatchQueries)
	}
	for i, query := range m.QueryParams {
		itemPath := fmt.Sprintf("%s.query_params[%d]", path, i)
		if query.Type != "" && query.Type != MeshValueMatchExact {
			return fmt.Errorf("%s.type must be Exact", itemPath)
		}
		if err := validateMeshHTTPName(query.Name); err != nil {
			return fmt.Errorf("%s.name: %w", itemPath, err)
		}
		if !utf8.ValidString(query.Value) {
			return fmt.Errorf("%s.value must be valid UTF-8", itemPath)
		}
		if length := utf8.RuneCountInString(query.Value); length < 1 || length > maxMeshQueryValue {
			return fmt.Errorf("%s.value must be between 1 and %d Unicode characters", itemPath, maxMeshQueryValue)
		}
	}
	return nil
}

func (p *MeshProbe) validate(path string) error {
	switch p.Type {
	case "", MeshProbeTypeTCP:
		if p.Host != "" || p.Path != "" {
			return fmt.Errorf("%s: TCP probe must not set host or path", path)
		}
	case MeshProbeTypeHTTP:
		if strings.TrimSpace(p.Host) == "" {
			return fmt.Errorf("%s.host is required for HTTP probe", path)
		}
		if p.Path == "" {
			return fmt.Errorf("%s.path is required for HTTP probe", path)
		}
		if err := validateMeshHTTPPath(p.Path); err != nil {
			return fmt.Errorf("%s.path: %w", path, err)
		}
	default:
		return fmt.Errorf("%s.type must be tcp or http", path)
	}
	return nil
}

func (o *MeshOriginTLS) validate(path string) error {
	if o.Verify && !o.Enabled {
		return fmt.Errorf("%s.verify requires enabled", path)
	}
	if len(o.ExtraCAFiles) > 0 && (!o.Enabled || !o.Verify) {
		return fmt.Errorf("%s.extra_ca_files requires enabled and verify", path)
	}
	return nil
}

func (s *MeshServer) validatePeers() error {
	seenIDs := make(map[string]int, len(s.Tunnel.Peering.Peers))
	seenAddresses := make(map[string]int, len(s.Tunnel.Peering.Peers))
	for i, peer := range s.Tunnel.Peering.Peers {
		path := fmt.Sprintf("tunnel.peering.peers[%d]", i)
		if err := validateMeshIdentity(path+".server_id", peer.ServerID); err != nil {
			return err
		}
		if peer.ServerID == s.ServerID {
			return fmt.Errorf("%s.server_id must not equal local server_id", path)
		}
		if previous, ok := seenIDs[peer.ServerID]; ok {
			return fmt.Errorf("%s.server_id duplicates tunnel.peering.peers[%d].server_id", path, previous)
		}
		seenIDs[peer.ServerID] = i
		if (peer.Address == "") != (peer.ServerName == "") {
			return fmt.Errorf("%s.address and server_name must be provided together", path)
		}
		if peer.Address == "" {
			continue
		}
		if err := ValidateAddress(peer.Address); err != nil {
			return fmt.Errorf("%s.address: %w", path, err)
		}
		if previous, ok := seenAddresses[peer.Address]; ok {
			return fmt.Errorf("%s.address duplicates tunnel.peering.peers[%d].address", path, previous)
		}
		seenAddresses[peer.Address] = i
	}
	if s.Tunnel.Peering.hasDialPeer() || s.Tunnel.Peering.hasOutboundConfig() {
		if err := s.Tunnel.Peering.Auth.Validate(); err != nil {
			return fmt.Errorf("tunnel.peering.auth: %w", err)
		}
		if err := s.Tunnel.Peering.TLS.Validate(s.Tunnel.Peering.Auth.Method); err != nil {
			return fmt.Errorf("tunnel.peering.tls: %w", err)
		}
	}
	return nil
}

func (s *MeshServer) validateIngress() error {
	for i, listener := range s.Ingress.Listeners {
		path := fmt.Sprintf("ingress.listeners[%d]", i)
		if err := validateListenerAddress(listener.Address); err != nil {
			return fmt.Errorf("%s.address: %w", path, err)
		}
		if err := listener.Capacity.Validate(path + ".capacity"); err != nil {
			return err
		}
		switch listener.Protocol {
		case MeshIngressProtocolHTTPS:
			if len(listener.Certificates) == 0 {
				return fmt.Errorf("%s.certificates must contain at least one certificate for https", path)
			}
		case MeshIngressProtocolHTTP, MeshIngressProtocolTLSPassthrough:
			if len(listener.Certificates) != 0 {
				return fmt.Errorf("%s.certificates are only valid for https", path)
			}
		default:
			return fmt.Errorf("%s.protocol must be http, https, or tls_passthrough", path)
		}
		if listener.Protocol == MeshIngressProtocolTLSPassthrough {
			if listener.MaxInflightRequests != nil {
				return fmt.Errorf("%s.max_inflight_requests is not valid for tls_passthrough", path)
			}
		} else if listener.MaxInflightRequests != nil && *listener.MaxInflightRequests < 0 {
			return fmt.Errorf("%s.max_inflight_requests must not be negative", path)
		}
		for certificateIndex, certificate := range listener.Certificates {
			certificatePath := fmt.Sprintf("%s.certificates[%d]", path, certificateIndex)
			if certificate.CertFile == "" || certificate.KeyFile == "" {
				return fmt.Errorf("%s.cert_file and key_file are required", certificatePath)
			}
		}
	}
	return nil
}

func (s *MeshServer) validateSocketClaims() error {
	type socketClaim struct {
		network string
		address string
	}
	claims := make(map[socketClaim]string, len(s.Ingress.Listeners)+2)
	claim := func(network, address, path string) error {
		key := socketClaim{network: network, address: address}
		if previous, ok := claims[key]; ok {
			return fmt.Errorf("%s conflicts with %s on %s socket %q", path, previous, network, address)
		}
		claims[key] = path
		return nil
	}
	if s.AdminAddress != "" {
		if err := claim("tcp", s.AdminAddress, "admin_address"); err != nil {
			return err
		}
	}
	if err := claim("udp", s.Tunnel.Listen.Address, "tunnel.listen.address"); err != nil {
		return err
	}
	for i, listener := range s.Ingress.Listeners {
		if err := claim("tcp", listener.Address, fmt.Sprintf("ingress.listeners[%d].address", i)); err != nil {
			return err
		}
	}
	return nil
}

func (p *MeshPeering) hasDialPeer() bool {
	for _, peer := range p.Peers {
		if peer.Address != "" {
			return true
		}
	}
	return false
}

func (p *MeshPeering) hasOutboundConfig() bool {
	return p.Auth.Method != "" || p.Auth.Token != "" || p.TLS != (ClientTLS{})
}

func validateMeshTunnelSettings(heartbeat, health time.Duration, copyBuffer int, path string) error {
	if heartbeat <= 0 {
		return fmt.Errorf("%s.heartbeat_interval must be positive", path)
	}
	if health <= 0 {
		return fmt.Errorf("%s.health_timeout must be positive", path)
	}
	if health <= heartbeat {
		return fmt.Errorf("%s.health_timeout (%v) must be greater than %s.heartbeat_interval (%v)", path, health, path, heartbeat)
	}
	if copyBuffer <= 0 {
		return fmt.Errorf("%s.tcp_copy_buffer_size must be positive", path)
	}
	return nil
}

func validateMeshServerTLS(tlsConfig ServerTLS, path string) error {
	if tlsConfig.ServerCertFile == "" {
		return fmt.Errorf("%s.server_cert_file is required", path)
	}
	if tlsConfig.ServerKeyFile == "" {
		return fmt.Errorf("%s.server_key_file is required", path)
	}
	if tlsConfig.SessionTicketEncryptionKeyRotationInterval < 0 {
		return fmt.Errorf("%s.session_ticket_encryption_key_rotation_interval must not be negative", path)
	}
	if tlsConfig.SessionTicketEncryptionKeyRotationInterval == 0 && tlsConfig.SessionTicketEncryptionKeyRotationOverlap != nil {
		return fmt.Errorf("%s.session_ticket_encryption_key_rotation_overlap must be omitted when session_ticket_encryption_key_rotation_interval is 0", path)
	}
	return nil
}

func validateMeshIdentity(path, value string) error {
	if len(value) < 1 || len(value) > maxMeshHostnameLength || !meshDNS1123SubdomainPattern.MatchString(value) {
		return fmt.Errorf("%s must be a lowercase DNS-1123 subdomain of 1..%d bytes", path, maxMeshHostnameLength)
	}
	for label := range strings.SplitSeq(value, ".") {
		if len(label) > 63 {
			return fmt.Errorf("%s contains a DNS label longer than 63 bytes", path)
		}
	}
	return nil
}

func validateMeshHostnames(path string, hostnames []string) error {
	if len(hostnames) > maxMeshRouteHostnames {
		return fmt.Errorf("%s must contain at most %d hostnames", path, maxMeshRouteHostnames)
	}
	for i, hostname := range hostnames {
		if err := validateMeshHostname(hostname); err != nil {
			return fmt.Errorf("%s[%d]: %w", path, i, err)
		}
	}
	return nil
}

func validateMeshHostname(hostname string) error {
	if len(hostname) < 1 || len(hostname) > maxMeshHostnameLength || !meshHostnamePattern.MatchString(hostname) {
		return fmt.Errorf("must be a lowercase RFC1123 hostname with an optional leading *.")
	}
	precise := strings.TrimPrefix(hostname, "*.")
	if net.ParseIP(precise) != nil {
		return errors.New("must not be an IP address")
	}
	for label := range strings.SplitSeq(precise, ".") {
		if len(label) > 63 {
			return errors.New("contains a DNS label longer than 63 bytes")
		}
	}
	return nil
}

func validateMeshHTTPName(name string) error {
	if len(name) < 1 || len(name) > maxMeshHTTPNameLength || !meshHTTPNamePattern.MatchString(name) {
		return fmt.Errorf("must be a valid HTTP token of 1..%d bytes", maxMeshHTTPNameLength)
	}
	return nil
}

func validateMeshHTTPPath(path string) error {
	if len(path) > maxMeshHTTPPathLength {
		return fmt.Errorf("must not exceed %d bytes", maxMeshHTTPPathLength)
	}
	if !strings.HasPrefix(path, "/") {
		return errors.New("must be an absolute path beginning with /")
	}
	if strings.Contains(path, "//") {
		return errors.New("must not contain //")
	}
	if strings.Contains(path, "/./") || strings.HasSuffix(path, "/.") {
		return errors.New("must not contain a dot path segment")
	}
	if strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") {
		return errors.New("must not contain a parent path segment")
	}
	for i := 0; i < len(path); i++ {
		character := path[i]
		if character == '%' {
			if i+2 >= len(path) || !isHex(path[i+1]) || !isHex(path[i+2]) {
				return errors.New("contains an invalid percent escape")
			}
			if path[i+1] == '2' && (path[i+2] == 'f' || path[i+2] == 'F') {
				return errors.New("must not contain an encoded slash")
			}
			i += 2
			continue
		}
		if character == '/' || isRFC3986PChar(character) {
			continue
		}
		return fmt.Errorf("contains invalid character %q", character)
	}
	return nil
}

func isRFC3986PChar(character byte) bool {
	if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
		return true
	}
	return strings.ContainsRune("-._~!$&'()*+,;=:@", rune(character))
}

func isHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F'
}

func validMeshHTTPMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return true
	default:
		return false
	}
}

func (g *MeshGroup) normalize() {
	for routeIndex := range g.Routes.HTTP {
		route := &g.Routes.HTTP[routeIndex]
		route.Hostnames = sortedUniqueStrings(route.Hostnames, true)
		for matchIndex := range route.Matches {
			match := &route.Matches[matchIndex]
			match.Headers = normalizeMeshHeaders(match.Headers)
			match.QueryParams = normalizeMeshQueries(match.QueryParams)
		}
	}
	if g.Routes.TLSPassthrough != nil {
		g.Routes.TLSPassthrough.Hostnames = sortedUniqueStrings(g.Routes.TLSPassthrough.Hostnames, true)
	}
}

func sortedUniqueStrings(values []string, foldCase bool) []string {
	result := make([]string, len(values))
	for i, value := range values {
		if foldCase {
			value = strings.ToLower(value)
		}
		result[i] = value
	}
	sort.Strings(result)
	return compactSortedStrings(result)
}

func compactSortedStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	if result == nil {
		return []string{}
	}
	return result
}

func normalizeMeshHeaders(headers []MeshHTTPHeaderMatch) []MeshHTTPHeaderMatch {
	result := make([]MeshHTTPHeaderMatch, 0, len(headers))
	for _, header := range headers {
		header.Name = strings.ToLower(header.Name)
		duplicate := false
		for _, existing := range result {
			if existing.Name == header.Name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, header)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Value < result[j].Value
	})
	return result
}

func normalizeMeshQueries(queries []MeshHTTPQueryParamMatch) []MeshHTTPQueryParamMatch {
	result := make([]MeshHTTPQueryParamMatch, 0, len(queries))
	for _, query := range queries {
		duplicate := false
		for _, existing := range result {
			if existing.Name == query.Name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, query)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Value < result[j].Value
	})
	return result
}

func loadMeshExtraCACertificates(paths []string) ([][]byte, error) {
	pemBegin := []byte("-----BEGIN ")
	certificates := make([][]byte, 0)
	for fileIndex, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: read certificate file: %w", fileIndex, err)
		}
		found := false
		for data = bytes.TrimSpace(data); len(data) > 0; data = bytes.TrimSpace(data) {
			if !bytes.HasPrefix(data, pemBegin) {
				return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: invalid PEM certificate data", fileIndex)
			}
			block, rest := pem.Decode(data)
			if block == nil || bytes.Count(data[:len(data)-len(rest)], pemBegin) != 1 {
				return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: invalid PEM certificate data", fileIndex)
			}
			data = rest
			if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: PEM block must be an unadorned CERTIFICATE", fileIndex)
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: parse certificate: %w", fileIndex, err)
			}
			certificates = append(certificates, bytes.Clone(certificate.Raw))
			found = true
		}
		if !found {
			return nil, fmt.Errorf("group.origin_tls.extra_ca_files[%d]: no certificate found", fileIndex)
		}
	}
	sort.Slice(certificates, func(i, j int) bool {
		return bytes.Compare(certificates[i], certificates[j]) < 0
	})
	result := certificates[:0]
	for _, certificate := range certificates {
		if len(result) == 0 || !bytes.Equal(result[len(result)-1], certificate) {
			result = append(result, certificate)
		}
	}
	if result == nil {
		return [][]byte{}, nil
	}
	return result, nil
}

// CanonicalBytes returns a copy of the public group declaration generated by
// LoadMeshClientConfig.
func (g *MeshGroup) CanonicalBytes() []byte {
	return bytes.Clone(g.canonicalBytes)
}

// ParseMeshGroupCanonical strictly validates one public wire declaration and
// returns only its identity; parsed route and certificate objects are discarded.
func ParseMeshGroupCanonical(data []byte) (string, uint64, error) {
	groupID, version, _, err := ParseMeshGroupCanonicalMeasured(data)
	return groupID, version, err
}

// MeshGroupValidationWork records the peak capacity of byte buffers explicitly
// owned by canonical validation. Decoder and x509 object allocations are not
// included.
type MeshGroupValidationWork struct {
	PeakBytes int64
}

type meshGroupWorkTracker struct {
	current int64
	peak    int64
	change  func(int64)
}

func (w *meshGroupWorkTracker) add(size int64) {
	w.current += size
	w.peak = max(w.peak, w.current)
	if w.change != nil {
		w.change(size)
	}
}

func (w *meshGroupWorkTracker) release(size int64) {
	w.current -= size
	if w.change != nil {
		w.change(-size)
	}
}

func ParseMeshGroupCanonicalMeasured(data []byte, change ...func(int64)) (_ string, _ uint64, work MeshGroupValidationWork, err error) {
	tracker := &meshGroupWorkTracker{}
	if len(change) > 0 {
		tracker.change = change[0]
	}
	defer func() { work.PeakBytes = tracker.peak }()
	var declaration meshGroupCanonical
	decodeErr := json.Unmarshal(data, &declaration, json.RejectUnknownMembers(true))
	var certificateBytes int64
	for _, der := range declaration.OriginTLS.ExtraCACertificates {
		certificateBytes += int64(cap(der))
	}
	tracker.add(certificateBytes)
	defer tracker.release(certificateBytes)
	if decodeErr != nil {
		return "", 0, work, fmt.Errorf("decode mesh group declaration: %w", decodeErr)
	}
	group := MeshGroup{
		GroupID:              declaration.GroupID,
		RuleVersion:          declaration.RuleVersion,
		Metric:               declaration.Metric,
		OutdatedClientPolicy: declaration.OutdatedClientPolicy,
		Probe: MeshProbe{
			Type: declaration.Probe.Type,
			Host: declaration.Probe.Host,
			Path: declaration.Probe.Path,
		},
		OriginTLS: MeshOriginTLS{
			Enabled:             declaration.OriginTLS.Enabled,
			Verify:              declaration.OriginTLS.Verify,
			ExtraCACertificates: declaration.OriginTLS.ExtraCACertificates,
		},
	}
	for _, route := range declaration.Routes.HTTP {
		converted := MeshHTTPRoute{Hostnames: route.Hostnames}
		for _, match := range route.Matches {
			convertedMatch := MeshHTTPMatch{
				Path:   &MeshHTTPPathMatch{Type: match.Path.Type, Value: match.Path.Value},
				Method: match.Method,
			}
			for _, header := range match.Headers {
				convertedMatch.Headers = append(convertedMatch.Headers, MeshHTTPHeaderMatch(header))
			}
			for _, query := range match.QueryParams {
				convertedMatch.QueryParams = append(convertedMatch.QueryParams, MeshHTTPQueryParamMatch(query))
			}
			converted.Matches = append(converted.Matches, convertedMatch)
		}
		group.Routes.HTTP = append(group.Routes.HTTP, converted)
	}
	if route := declaration.Routes.TLSPassthrough; route != nil {
		group.Routes.TLSPassthrough = &MeshTLSRoute{Hostnames: route.Hostnames}
	}
	group.ApplyDefaults()
	if err := group.Validate("group"); err != nil {
		return "", 0, work, err
	}
	if len(group.OriginTLS.ExtraCACertificates) > 0 && (!group.OriginTLS.Enabled || !group.OriginTLS.Verify) {
		return "", 0, work, errors.New("group.origin_tls.extra_ca_certificates requires enabled and verify")
	}
	for _, der := range group.OriginTLS.ExtraCACertificates {
		if _, err := x509.ParseCertificate(der); err != nil {
			return "", 0, work, fmt.Errorf("invalid mesh group extra CA: %w", err)
		}
	}
	sort.Slice(group.OriginTLS.ExtraCACertificates, func(i, j int) bool {
		return bytes.Compare(group.OriginTLS.ExtraCACertificates[i], group.OriginTLS.ExtraCACertificates[j]) < 0
	})
	group.OriginTLS.ExtraCACertificates = slices.CompactFunc(group.OriginTLS.ExtraCACertificates, bytes.Equal)
	group.normalize()
	canonical, err := marshalMeshGroupCanonicalMeasured(group, tracker)
	if err != nil {
		return "", 0, work, fmt.Errorf("canonicalize mesh group declaration: %w", err)
	}
	tracker.add(int64(cap(canonical)))
	defer tracker.release(int64(cap(canonical)))
	if !bytes.Equal(canonical, data) {
		return "", 0, work, errors.New("mesh group declaration is not canonical")
	}
	return group.GroupID, group.RuleVersion, work, nil
}

type meshGroupCanonical struct {
	GroupID              string                 `json:"group_id"`
	RuleVersion          uint64                 `json:"rule_version"`
	Metric               int64                  `json:"metric"`
	OutdatedClientPolicy string                 `json:"outdated_client_policy"`
	Routes               meshRoutesCanonical    `json:"routes"`
	Probe                meshProbeCanonical     `json:"probe"`
	OriginTLS            meshOriginTLSCanonical `json:"origin_tls"`
}

type meshRoutesCanonical struct {
	HTTP           []meshHTTPRouteCanonical `json:"http"`
	TLSPassthrough *meshTLSRouteCanonical   `json:"tls_passthrough"`
}

type meshHTTPRouteCanonical struct {
	Hostnames []string                 `json:"hostnames"`
	Matches   []meshHTTPMatchCanonical `json:"matches"`
}

type meshTLSRouteCanonical struct {
	Hostnames []string `json:"hostnames"`
}

type meshHTTPMatchCanonical struct {
	Path        meshHTTPPathCanonical         `json:"path"`
	Method      string                        `json:"method"`
	Headers     []meshHTTPHeaderCanonical     `json:"headers"`
	QueryParams []meshHTTPQueryParamCanonical `json:"query_params"`
}

type meshHTTPPathCanonical struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type meshHTTPHeaderCanonical struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type meshHTTPQueryParamCanonical struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type meshProbeCanonical struct {
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
}

type meshOriginTLSCanonical struct {
	Enabled             bool     `json:"enabled"`
	Verify              bool     `json:"verify"`
	ExtraCACertificates [][]byte `json:"extra_ca_certificates"`
}

func marshalMeshGroupCanonical(group MeshGroup) ([]byte, error) {
	return marshalMeshGroupCanonicalMeasured(group, nil)
}

func marshalMeshGroupCanonicalMeasured(group MeshGroup, tracker *meshGroupWorkTracker) ([]byte, error) {
	httpRoutes := make([]meshHTTPRouteCanonical, 0, len(group.Routes.HTTP))
	for _, route := range group.Routes.HTTP {
		matches := make([]meshHTTPMatchCanonical, 0, len(route.Matches))
		for _, match := range route.Matches {
			headers := make([]meshHTTPHeaderCanonical, 0, len(match.Headers))
			for _, header := range match.Headers {
				headers = append(headers, meshHTTPHeaderCanonical{Type: header.Type, Name: header.Name, Value: header.Value})
			}
			queries := make([]meshHTTPQueryParamCanonical, 0, len(match.QueryParams))
			for _, query := range match.QueryParams {
				queries = append(queries, meshHTTPQueryParamCanonical{Type: query.Type, Name: query.Name, Value: query.Value})
			}
			path := meshHTTPPathCanonical{Type: MeshPathMatchPathPrefix, Value: "/"}
			if match.Path != nil {
				path = meshHTTPPathCanonical{Type: match.Path.Type, Value: match.Path.Value}
			}
			matches = append(matches, meshHTTPMatchCanonical{
				Path:        path,
				Method:      match.Method,
				Headers:     headers,
				QueryParams: queries,
			})
		}
		var err error
		matches, err = sortedUniqueCanonical(matches, tracker)
		if err != nil {
			return nil, err
		}
		httpRoutes = append(httpRoutes, meshHTTPRouteCanonical{Hostnames: nonNilStrings(route.Hostnames), Matches: matches})
	}
	var err error
	httpRoutes, err = sortedUniqueCanonical(httpRoutes, tracker)
	if err != nil {
		return nil, err
	}

	var tlsRoute *meshTLSRouteCanonical
	if group.Routes.TLSPassthrough != nil {
		tlsRoute = &meshTLSRouteCanonical{Hostnames: nonNilStrings(group.Routes.TLSPassthrough.Hostnames)}
	}
	extraCAs := group.OriginTLS.ExtraCACertificates
	if extraCAs == nil {
		extraCAs = [][]byte{}
	}
	projection := meshGroupCanonical{
		GroupID:              group.GroupID,
		RuleVersion:          group.RuleVersion,
		Metric:               group.Metric,
		OutdatedClientPolicy: group.OutdatedClientPolicy,
		Routes:               meshRoutesCanonical{HTTP: httpRoutes, TLSPassthrough: tlsRoute},
		Probe:                meshProbeCanonical{Type: group.Probe.Type, Host: group.Probe.Host, Path: group.Probe.Path},
		OriginTLS: meshOriginTLSCanonical{
			Enabled:             group.OriginTLS.Enabled,
			Verify:              group.OriginTLS.Verify,
			ExtraCACertificates: extraCAs,
		},
	}
	return json.Marshal(projection)
}

func sortedUniqueCanonical[T any](values []T, tracker *meshGroupWorkTracker) ([]T, error) {
	type keyedValue struct {
		value T
		key   []byte
	}
	keyed := make([]keyedValue, 0, len(values))
	var keyBytes int64
	if tracker != nil {
		defer func() { tracker.release(keyBytes) }()
	}
	for _, value := range values {
		key, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if tracker != nil {
			capacity := int64(cap(key))
			tracker.add(capacity)
			keyBytes += capacity
		}
		keyed = append(keyed, keyedValue{value: value, key: key})
	}
	sort.Slice(keyed, func(i, j int) bool {
		return bytes.Compare(keyed[i].key, keyed[j].key) < 0
	})
	result := make([]T, 0, len(keyed))
	var previous []byte
	for _, item := range keyed {
		if previous == nil || !bytes.Equal(previous, item.key) {
			result = append(result, item.value)
			previous = item.key
		}
	}
	return result, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
