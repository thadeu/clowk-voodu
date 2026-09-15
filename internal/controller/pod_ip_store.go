package controller

import (
	"context"
	"fmt"
	"strconv"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// podRef is what an address reservation points back to.
func podRef(scope, name string, ordinal int) string {
	return scope + "/" + name + "/" + strconv.Itoa(ordinal)
}

func (s *EtcdStore) ReservePodIP(ctx context.Context, scope, name string, ordinal int, ip string) (bool, error) {
	addrKey := PodIPAddrKey(ip)
	podKey := PodIPPodKey(scope, name, ordinal)

	resp, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.CreateRevision(addrKey), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(podKey), "=", 0),
		).
		Then(
			clientv3.OpPut(addrKey, podRef(scope, name, ordinal)),
			clientv3.OpPut(podKey, ip),
		).
		Commit()
	if err != nil {
		return false, fmt.Errorf("etcd reserve pod ip: %w", err)
	}

	return resp.Succeeded, nil
}

func (s *EtcdStore) GetPodIP(ctx context.Context, scope, name string, ordinal int) (string, error) {
	resp, err := s.client.Get(ctx, PodIPPodKey(scope, name, ordinal))
	if err != nil {
		return "", fmt.Errorf("etcd get pod ip: %w", err)
	}

	if len(resp.Kvs) == 0 {
		return "", nil
	}

	return string(resp.Kvs[0].Value), nil
}

// ReleasePodIP deletes the pod's key, and the address key only while it
// still points at this pod — a release never frees an address someone
// else holds.
func (s *EtcdStore) ReleasePodIP(ctx context.Context, scope, name string, ordinal int) error {
	podKey := PodIPPodKey(scope, name, ordinal)

	resp, err := s.client.Get(ctx, podKey)
	if err != nil {
		return fmt.Errorf("etcd get pod ip: %w", err)
	}

	if len(resp.Kvs) == 0 {
		return nil
	}

	addrKey := PodIPAddrKey(string(resp.Kvs[0].Value))

	_, err = s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.Value(addrKey), "=", podRef(scope, name, ordinal))).
		Then(clientv3.OpDelete(podKey), clientv3.OpDelete(addrKey)).
		Else(clientv3.OpDelete(podKey)).
		Commit()
	if err != nil {
		return fmt.Errorf("etcd release pod ip: %w", err)
	}

	return nil
}

func (s *EtcdStore) ReleasePodIPs(ctx context.Context, scope, name string) error {
	resp, err := s.client.Get(ctx, PodIPPodPrefix(scope, name), clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return fmt.Errorf("etcd list pod ips: %w", err)
	}

	prefix := PodIPPodPrefix(scope, name)

	for _, kv := range resp.Kvs {
		ordinal, err := strconv.Atoi(string(kv.Key)[len(prefix):])
		if err != nil {
			continue
		}

		if err := s.ReleasePodIP(ctx, scope, name, ordinal); err != nil {
			return err
		}
	}

	return nil
}
