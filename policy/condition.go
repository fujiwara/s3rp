package policy

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// SourceIPKey is the condition key naming the client's source address. The
// AWS spelling is used because every surveyed S3-compatible implementation
// (Ceph RGW, MinIO) accepts it, so policies written for them work unchanged.
// Key names are matched case-insensitively, as on AWS. Condition syntax is
// the same in every Dialect; a key-renaming hook is a possible future
// extension, deliberately not provided now.
const SourceIPKey = "aws:SourceIp"

// Condition operator names, matched exactly as AWS spells them.
const (
	opIPAddress    = "IpAddress"
	opNotIPAddress = "NotIpAddress"
)

// RequestContext carries the request-constant facts condition operators
// test. The zero value means the source is unknown, which fails closed:
// IpAddress never matches, NotIpAddress always matches.
type RequestContext struct {
	// SourceIP is the client's source address; a zero Addr means unknown.
	SourceIP netip.Addr
}

// Condition restricts a statement to requests whose context matches every
// operator present: values within one operator are ORed, operators are
// ANDed. Only the IpAddress and NotIpAddress operators on the SourceIPKey
// key are supported; unknown operators and keys are rejected at parse time
// rather than ignored, so a restriction the author intended can never be
// silently dropped. Values are CIDR prefixes or plain addresses (a /32 or
// /128 prefix). IPv4 and IPv6 are distinct families: an IPv4 prefix never
// matches an IPv6 source and vice versa (IPv4-mapped IPv6 sources are
// unmapped before matching, so they count as IPv4).
type Condition struct {
	IPAddress    StringOrSlice // IpAddress operator values, as written
	NotIPAddress StringOrSlice // NotIpAddress operator values, as written

	// precompiled by compile: the parsed prefixes. invalid marks a value
	// that does not parse, only reachable on a hand-built policy that
	// bypassed Parse; it fails closed in Statement.matchCondition.
	ipAddress    []netip.Prefix
	notIPAddress []netip.Prefix
	compiled     bool
	invalid      bool
}

func (c *Condition) UnmarshalJSON(data []byte) error {
	var raw map[string]map[string]StringOrSlice
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("malformed condition: %w", err)
	}
	for op, keys := range raw {
		var dst *StringOrSlice
		switch op {
		case opIPAddress:
			dst = &c.IPAddress
		case opNotIPAddress:
			dst = &c.NotIPAddress
		default:
			return fmt.Errorf("unknown condition operator %q (only %s and %s are supported)",
				op, opIPAddress, opNotIPAddress)
		}
		for key, values := range keys {
			if !strings.EqualFold(key, SourceIPKey) {
				return fmt.Errorf("unknown condition key %q (only %s is supported)", key, SourceIPKey)
			}
			*dst = append(*dst, values...)
		}
		if len(*dst) == 0 {
			return fmt.Errorf("condition operator %s has no values", op)
		}
	}
	return nil
}

func (c *Condition) MarshalJSON() ([]byte, error) {
	m := make(map[string]map[string]StringOrSlice, 2)
	if len(c.IPAddress) > 0 {
		m[opIPAddress] = map[string]StringOrSlice{SourceIPKey: c.IPAddress}
	}
	if len(c.NotIPAddress) > 0 {
		m[opNotIPAddress] = map[string]StringOrSlice{SourceIPKey: c.NotIPAddress}
	}
	return json.Marshal(m)
}

// compile parses the operator values into prefixes once, so matching does
// not re-parse them per request. On error the condition is marked invalid,
// which fails closed in Statement.matchCondition; Parse surfaces the error
// to the author instead.
func (c *Condition) compile() error {
	c.compiled = true
	var err error
	if c.ipAddress, err = parsePrefixes(c.IPAddress); err == nil {
		c.notIPAddress, err = parsePrefixes(c.NotIPAddress)
	}
	if err != nil {
		c.invalid = true
	}
	return err
}

func parsePrefixes(values []string) ([]netip.Prefix, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, len(values))
	for i, v := range values {
		p, err := parseIPValue(v)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

// parseIPValue parses one operator value: a CIDR prefix, or a plain address
// treated as a /32 (IPv4) or /128 (IPv6) prefix. Zoned addresses are
// rejected — a zone is local to one host and meaningless in a policy.
func parseIPValue(v string) (netip.Prefix, error) {
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid source IP value %q", v)
		}
		return p, nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("invalid source IP value %q", v)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// match reports whether the request context satisfies the condition. An
// unknown source fails closed on both operators: IpAddress does not match
// (an Allow gated on it grants nothing) and NotIpAddress matches (a Deny
// gated on it applies).
func (c *Condition) match(rc RequestContext) bool {
	if len(c.ipAddress) > 0 &&
		!(rc.SourceIP.IsValid() && prefixesContain(c.ipAddress, rc.SourceIP)) {
		return false
	}
	// NotIpAddress fails only for a known source inside the range; an
	// unknown source keeps the condition holding, so a Deny gated on it
	// still applies.
	if len(c.notIPAddress) > 0 &&
		rc.SourceIP.IsValid() && prefixesContain(c.notIPAddress, rc.SourceIP) {
		return false
	}
	return true
}

func prefixesContain(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
