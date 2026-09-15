// wire_store.go is the etcd side of WireGuard peers: one JSON record per
// peer under /wire/peers/<address>. Mirrored by memstore_test.go.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *EtcdStore) PutWirePeer(ctx context.Context, p WirePeer) error {
	if p.Address == "" {
		return fmt.Errorf("etcd put wire peer: empty address")
	}

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal wire peer %s: %w", p.Address, err)
	}

	if _, err := s.client.Put(ctx, WirePeerKey(p.Address), string(data)); err != nil {
		return fmt.Errorf("etcd put wire peer %s: %w", p.Address, err)
	}

	return nil
}

func (s *EtcdStore) ListWirePeers(ctx context.Context) ([]WirePeer, error) {
	resp, err := s.client.Get(ctx, WirePeersPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd list wire peers: %w", err)
	}

	out := make([]WirePeer, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		var p WirePeer

		if err := json.Unmarshal(kv.Value, &p); err != nil {
			return nil, fmt.Errorf("decode wire peer %s: %w", string(kv.Key), err)
		}

		out = append(out, p)
	}

	return out, nil
}

func (s *EtcdStore) DeleteWirePeer(ctx context.Context, address string) (bool, error) {
	if address == "" {
		return false, nil
	}

	resp, err := s.client.Delete(ctx, WirePeerKey(address))
	if err != nil {
		return false, fmt.Errorf("etcd delete wire peer %s: %w", address, err)
	}

	return resp.Deleted > 0, nil
}
