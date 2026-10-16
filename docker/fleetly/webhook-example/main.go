// webhook-example —— torchwood Webhooks 接收端示例（docs/developer/08-functions.md §7.4）。
//
// 职责单一：验签（X-Tw-Signature）→ 记录投递 → 200。部署为 fleetly app 后，
// 把它的公网 Route URL 登记为 torchwood webhook 的 target，即可端到端演示
// 「平台事件 → CloudEvents 投递 → 签名校验 → 接收方业务」全链。
//
// 验签口径与 sdk/go/functions（FDK）逐字节一致：载荷串 "<t>.<body>"、
// HMAC-SHA256 hex、头格式 t=/v1=/v2=、±5 分钟窗、常数时间比较、轮换宽限窗
// 内旧钥 v2 并存。生产接收方优先直接复用 FDK（functions.Verify / Verify 中
// 间件）；本示例为保持单文件零依赖而内联同款实现。
//
// 运行环境：
//
//	WEBHOOK_SECRET  必填。torchwood webhooks create / rotate-secret 一次性
//	                回显的 whsec_ 明文。缺失即启动失败（fail-closed：无密钥
//	                等于不设防）。
//	PORT            监听端口（缺省 8080）。
//
// 面：
//
//	POST /hook        torchwood webhook target（验签必经，失败 401 → 平台按
//	                  重试梯度重投）。
//	GET  /healthz     存活/就绪探针（不走验签）。
//	GET  /deliveries  最近投递的 JSON 视图（演示用；生产接收方不要裸暴露）。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hdrSignature = "X-Tw-Signature"
	maxBodyBytes = 4 << 20
	sigTolerance = 5 * time.Minute
	// retainedDeliveries 是 /deliveries 视图的内存环容量（演示用途，进程内）。
	retainedDeliveries = 100
)

type delivery struct {
	ReceivedAt time.Time         `json:"received_at"`
	EventType  string            `json:"event_type"`
	EventID    string            `json:"event_id"`
	Seq        string            `json:"twseq,omitempty"`
	Payload    json.RawMessage   `json:"payload"`
	Headers    map[string]string `json:"headers,omitempty"`
}

type store struct {
	mu   sync.Mutex
	ring []delivery
}

func (s *store) add(d delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(s.ring, d)
	if len(s.ring) > retainedDeliveries {
		s.ring = s.ring[len(s.ring)-retainedDeliveries:]
	}
}

func (s *store) snapshot() []delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]delivery, len(s.ring))
	copy(out, s.ring)
	// 新投递在前，读面友好。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// verifySignature 与 FDK VerifySignatureHeader 同口径（见文件头注）。
func verifySignature(header string, body []byte, secrets []string, now time.Time) error {
	if header == "" {
		return errors.New("missing X-Tw-Signature header")
	}
	var timestamp int64
	sigs := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" || v == "" {
			continue
		}
		if k == "t" {
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid timestamp %q", v)
			}
			timestamp = parsed
			continue
		}
		sigs[k] = v
	}
	if timestamp == 0 {
		return errors.New("missing t in signature header")
	}
	if len(sigs) == 0 {
		return errors.New("no signature entries")
	}
	delta := now.Sub(time.Unix(timestamp, 0))
	if delta < -sigTolerance || delta > sigTolerance {
		return fmt.Errorf("timestamp %d outside tolerance", timestamp)
	}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		for _, got := range sigs {
			if hmac.Equal([]byte(want), []byte(got)) {
				return nil
			}
		}
	}
	return errors.New("no signature match")
}

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("WEBHOOK_SECRET is required (the whsec_ secret echoed once by torchwood webhooks create/rotate-secret)")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	s := &store{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxBodyBytes {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := verifySignature(r.Header.Get(hdrSignature), body, []string{secret}, time.Now()); err != nil {
			// 401 → torchwood 按重试梯度重投；接收方排障看这条日志。
			log.Printf("signature verification failed: %v", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		d := delivery{ReceivedAt: time.Now().UTC(), Payload: json.RawMessage(body)}
		var envelope struct {
			Type      string         `json:"type"`
			ID        string         `json:"id"`
			Twproject string         `json:"twproject"`
			Twseq     json.Number    `json:"twseq"`
			Data      map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			d.EventType = envelope.Type
			d.EventID = envelope.ID
			d.Seq = envelope.Twseq.String()
		}
		s.add(d)
		log.Printf("delivered: type=%s id=%s twseq=%s bytes=%d", d.EventType, d.EventID, d.Seq, len(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"received":true}`)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /deliveries", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.snapshot()); err != nil {
			http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		}
	})

	addr := ":" + port
	log.Printf("webhook-example listening on %s (signature verification on)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
