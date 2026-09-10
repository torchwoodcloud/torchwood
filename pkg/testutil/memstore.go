package testutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
)

// memObject 是内存对象（data + Put 时间戳，供 List/LastModified 语义）。
type memObject struct {
	data     []byte
	modified time.Time
}

// MemObjectStore is an in-memory ObjectStore for integration tests.
type MemObjectStore struct {
	mu      sync.Mutex
	buckets map[string]map[string]memObject
}

func NewMemObjectStore() *MemObjectStore {
	return &MemObjectStore{buckets: map[string]map[string]memObject{}}
}

func (m *MemObjectStore) EnsureBucket(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[name]; !ok {
		m.buckets[name] = map[string]memObject{}
	}
	return nil
}

func (m *MemObjectStore) Put(_ context.Context, bucket, key string, data io.Reader, size int64, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[bucket]; !ok {
		m.buckets[bucket] = map[string]memObject{}
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(data, buf); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	m.buckets[bucket][key] = memObject{data: bytes.Clone(buf), modified: time.Now()}
	return nil
}

func (m *MemObjectStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.buckets[bucket]
	if !ok {
		return nil, fmt.Errorf("object not found: %s/%s", bucket, key)
	}
	obj, ok := objects[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s/%s", bucket, key)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(obj.data))), nil
}

func (m *MemObjectStore) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if objects, ok := m.buckets[bucket]; ok {
		delete(objects, key)
	}
	return nil
}

// Compose 按序拼接 srcKeys 字节写 dstKey（测试语义，不强制 5MiB/10000 约束）。
func (m *MemObjectStore) Compose(_ context.Context, bucket, dstKey string, srcKeys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.buckets[bucket]
	if !ok {
		return fmt.Errorf("bucket not found: %s", bucket)
	}
	var buf bytes.Buffer
	for _, k := range srcKeys {
		obj, ok := objects[k]
		if !ok {
			return fmt.Errorf("object not found: %s/%s", bucket, k)
		}
		buf.Write(obj.data)
	}
	objects[dstKey] = memObject{data: bytes.Clone(buf.Bytes()), modified: time.Now()}
	return nil
}

// List 列出 bucket 下指定前缀的对象（recursive 语义，内存实现天然全量）。
func (m *MemObjectStore) List(_ context.Context, bucket, prefix string) ([]domainstorage.ObjectMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.buckets[bucket]
	if !ok {
		return nil, nil
	}
	var out []domainstorage.ObjectMeta
	for k, obj := range objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, domainstorage.ObjectMeta{Key: k, LastModified: obj.modified})
		}
	}
	return out, nil
}

// SetObjectTime 调整对象 LastModified（测试辅助：模拟历史分片对象）。
func (m *MemObjectStore) SetObjectTime(bucket, key string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	objects, ok := m.buckets[bucket]
	if !ok {
		return fmt.Errorf("object not found: %s/%s", bucket, key)
	}
	obj, ok := objects[key]
	if !ok {
		return fmt.Errorf("object not found: %s/%s", bucket, key)
	}
	obj.modified = t
	objects[key] = obj
	return nil
}

func (m *MemObjectStore) Ping(_ context.Context) error {
	return nil
}

var _ domainstorage.ObjectStore = (*MemObjectStore)(nil)
