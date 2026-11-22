package collab

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
)

// IPSetRef names one WAF IP set (the ARN Terraform created).
type IPSetRef struct {
	ARN string
	// IPv6 is the address family of the set.
	IPv6 bool
}

// IPSets reads and replaces the addresses of WAF IP sets. Update is a full replacement.
type IPSets interface {
	Addresses(ctx context.Context, ref IPSetRef) ([]string, error)
	Update(ctx context.Context, ref IPSetRef, addrs []string) error
}

// CIDRSource reads collab.allowed_source_cidrs.
type CIDRSource interface {
	AllowedCIDRs(ctx context.Context) ([]string, error)
}

// WAFSync keeps the two WAF IP sets equal to collab.allowed_source_cidrs (RC-28). An empty list means the
// whole internet (0.0.0.0/0 and ::/0), so the WAF allow rule always holds; the edge still decides access.
type WAFSync struct {
	CIDRs CIDRSource
	Sets  IPSets
	V4    IPSetRef
	V6    IPSetRef
}

// Split divides cidrs by address family, canonicalizing each prefix; an empty family gets the wildcard of
// that family only when the whole list is empty (a list of only IPv4 prefixes leaves IPv6 closed).
func Split(cidrs []string) (v4, v6 []string, err error) {
	if len(cidrs) == 0 {
		return []string{"0.0.0.0/0"}, []string{"::/0"}, nil
	}
	for _, c := range cidrs {
		p, perr := netip.ParsePrefix(c)
		if perr != nil {
			return nil, nil, fmt.Errorf("collab: allowed source CIDR %q: %w", c, perr)
		}
		p = p.Masked()
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	slices.Sort(v4)
	slices.Sort(v6)
	return slices.Compact(v4), slices.Compact(v6), nil
}

// Run syncs once; a set that already holds the wanted addresses is not written.
func (w *WAFSync) Run(ctx context.Context) error {
	cidrs, err := w.CIDRs.AllowedCIDRs(ctx)
	if err != nil {
		return fmt.Errorf("collab: read allowed source CIDRs: %w", err)
	}
	v4, v6, err := Split(cidrs)
	if err != nil {
		return err
	}
	for _, s := range []struct {
		ref  IPSetRef
		want []string
	}{{w.V4, v4}, {w.V6, v6}} {
		if s.ref.ARN == "" {
			continue
		}
		have, err := w.Sets.Addresses(ctx, s.ref)
		if err != nil {
			return fmt.Errorf("collab: read IP set: %w", err)
		}
		slices.Sort(have)
		if slices.Equal(have, s.want) {
			continue
		}
		if err := w.Sets.Update(ctx, s.ref, s.want); err != nil {
			return fmt.Errorf("collab: update IP set: %w", err)
		}
	}
	return nil
}
