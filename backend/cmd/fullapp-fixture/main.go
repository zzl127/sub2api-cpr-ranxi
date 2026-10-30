//go:build fullappfixture

// CI fixture only. Never linked into cmd/server or a production release.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

type fixtureInput struct {
	Database      config.DatabaseConfig `json:"database"`
	Redis         config.RedisConfig    `json:"redis"`
	APIKey        string                `json:"api_key"`
	Origin        string                `json:"origin"`
	UserID        int64                 `json:"user_id"`
	APIKeyID      int64                 `json:"api_key_id"`
	AccountID     int64                 `json:"account_id"`
	PeerAccountID int64                 `json:"peer_account_id"`
}

type fixtureOutput struct {
	UserID             int64   `json:"user_id"`
	APIKeyID           int64   `json:"api_key_id"`
	AccountID          int64   `json:"account_id"`
	UsageRows          int64   `json:"usage_rows"`
	DedupRows          int64   `json:"dedup_rows"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	Balance            float64 `json:"balance"`
	UserSlots          int     `json:"user_slots"`
	AccountSlots       int     `json:"account_slots"`
	APIKeySlots        int     `json:"api_key_slots"`
	PeerAccountID      int64   `json:"peer_account_id"`
	PeerUsageRows      int64   `json:"peer_usage_rows"`
	PeerInputTokens    int64   `json:"peer_input_tokens"`
	PeerOutputTokens   int64   `json:"peer_output_tokens"`
	PeerAccountSlots   int     `json:"peer_account_slots"`
	APIKeyQueueActive  int     `json:"api_key_queue_active"`
	APIKeyQueueWaiting int     `json:"api_key_queue_waiting"`
	WSTurns            int     `json:"ws_turns,omitempty"`
}

var fixtureStage = "input"

func main() {
	// Repository failures can contain driver details. Only a fixed outer error
	// is returned; no SQL/credentials/config/error strings enter fixture logs.
	log.SetOutput(io.Discard)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if os.Getenv("CI_FULLAPP_FIXTURE") != "1" || len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "explicit isolated CI fixture required")
		os.Exit(2)
	}
	if os.Args[1] == "serve" {
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "synthetic upstream stopped")
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "isolated seed or observation failed stage="+fixtureStage)
		os.Exit(1)
	}
}

func run(op string) error {
	var in fixtureInput
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return err
	}
	// Reject arbitrary existing endpoints even if somebody invokes this tool.
	if !strings.HasPrefix(in.Database.Host, "s2-bootstrap-") || !strings.HasSuffix(in.Database.Host, "-pg") ||
		in.Database.DBName != "bootstrap_fixture" || in.Database.Port != 5432 || in.Database.SSLMode != "disable" ||
		!strings.HasPrefix(in.Redis.Host, "s2-bootstrap-") || !strings.HasSuffix(in.Redis.Host, "-redis") || in.Redis.Port != 6379 || in.Redis.DB != 0 {
		return errors.New("fixture endpoints required")
	}
	prefix := strings.TrimSuffix(in.Database.Host, "-pg")
	if strings.TrimSuffix(in.Redis.Host, "-redis") != prefix {
		return errors.New("one fixture namespace required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := timezone.Init("UTC"); err != nil {
		return err
	}
	db, err := sql.Open("postgres", in.Database.DSNWithTimezone("UTC"))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(2)
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	cache := redis.NewClient(&redis.Options{Addr: in.Redis.Address(), MaxRetries: -1,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	defer cache.Close()
	var out fixtureOutput
	switch op {
	case "seed-admin-ui":
		// Test data only, inside the validated disposable fixture database.
		// Do not invoke an agreement endpoint or alter any production guard.
		fixtureStage = "synthetic-admin-ui-state"
		var adminID int64
		if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE email=$1 AND role=$2 AND deleted_at IS NULL`,
			"admin-fixture@example.invalid", service.RoleAdmin).Scan(&adminID); err != nil {
			return err
		}
		ack, err := json.Marshal(service.AdminComplianceAcknowledgement{
			Version: service.AdminComplianceVersion, AdminUserID: adminID,
			UserAgent: "synthetic-fixture-only", AcceptedAt: time.Unix(1700000000, 0).UTC(),
		})
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO settings (key,value) VALUES ($1,$2)
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`,
			fmt.Sprintf("admin_compliance_acknowledgement:%d", adminID), string(ack)); err != nil {
			return err
		}
	case "probe-cpr-ws":
		if !strings.HasPrefix(in.APIKey, "sk-fixture-") {
			return errors.New("synthetic key required")
		}
		fixtureStage = "websocket-two-turns"
		if err := probeCPRWebSocket(ctx, prefix, in.APIKey); err != nil {
			return err
		}
		out.WSTurns = 2
	case "seed", "seed-multi", "seed-cpr":
		if in.Origin != "http://"+prefix+"-model:9090" ||
			!strings.HasPrefix(in.APIKey, "sk-fixture-") {
			return errors.New("synthetic model/key required")
		}
		fixtureStage = "create-user"
		user, err := client.User.Create().SetEmail("model-fixture@example.invalid").
			SetPasswordHash("not-a-real-login-hash").SetRole(service.RoleUser).
			SetStatus(service.StatusActive).SetBalance(10).SetConcurrency(2).Save(ctx)
		if err != nil {
			return err
		}
		fixtureStage = "create-group"
		group, err := client.Group.Create().SetName("model-fixture").SetPlatform(service.PlatformOpenAI).
			SetSubscriptionType(service.SubscriptionTypeStandard).SetStatus(service.StatusActive).
			SetRateMultiplier(1).Save(ctx)
		if err != nil {
			return err
		}
		key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: in.APIKey,
			Name: "model-fixture", Status: service.StatusActive, ConcurrencyLimit: 2}
		fixtureStage = "create-key"
		if err := repository.NewAPIKeyRepository(client, db).Create(ctx, key); err != nil {
			return err
		}
		snapshots := repository.NewSchedulerCache(cache)
		accounts := repository.NewAccountRepository(client, db, snapshots)
		account := &service.Account{Name: "model-fixture", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Concurrency: 2, Priority: 1, QuotaDimension: "global",
			Credentials: map[string]any{"api_key": "synthetic-upstream-only", "base_url": in.Origin,
				"model_mapping": map[string]string{"gpt-6.1-sol": "gpt-6.1-sol"}},
			Extra: map[string]any{"openai_passthrough": true}}
		if op == "seed-cpr" {
			account.Type = service.AccountTypeCPR
			account.Credentials = map[string]any{"api_key": "synthetic-upstream-only"}
		}
		fixtureStage = "create-account-group"
		atomicCreator, ok := accounts.(service.AccountDuplicateRepository)
		if !ok {
			return errors.New("fixture requires atomic account/group repository capability")
		}
		if err := atomicCreator.CreateWithAccountGroups(ctx, account, []service.AccountGroup{{GroupID: group.ID, Priority: 1}}); err != nil {
			return err
		}
		fixtureStage = "hydrate-account"
		actual, err := accounts.GetByID(ctx, account.ID)
		if err != nil {
			return err
		}
		if !actual.IsSchedulable() || actual.ParentAccountID != nil || actual.QuotaDimension != "global" {
			return errors.New("root account eligibility not preserved")
		}
		snapshotAccounts := []service.Account{*actual}
		if op == "seed-multi" {
			peer := &service.Account{Name: "model-fixture-peer", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
				Concurrency: 2, Priority: 1, QuotaDimension: "global",
				Credentials: map[string]any{"api_key": "synthetic-upstream-peer", "base_url": in.Origin,
					"model_mapping": map[string]string{"gpt-6.1-sol": "gpt-6.1-sol"}},
				Extra: map[string]any{"openai_passthrough": true}}
			fixtureStage = "create-account-group"
			if err := atomicCreator.CreateWithAccountGroups(ctx, peer, []service.AccountGroup{{GroupID: group.ID, Priority: 1}}); err != nil {
				return err
			}
			fixtureStage = "hydrate-account"
			actualPeer, err := accounts.GetByID(ctx, peer.ID)
			if err != nil {
				return err
			}
			if !actualPeer.IsSchedulable() || actualPeer.ParentAccountID != nil || actualPeer.QuotaDimension != "global" {
				return errors.New("peer root account eligibility not preserved")
			}
			snapshotAccounts = append(snapshotAccounts, *actualPeer)
			out.PeerAccountID = peer.ID
		}
		fixtureStage = "publish-snapshot"
		bucket := service.SchedulerBucket{GroupID: group.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
		token, err := snapshots.CaptureBucketWriteToken(ctx, bucket)
		if err != nil {
			return err
		}
		if err := snapshots.SetSnapshot(ctx, bucket, token, snapshotAccounts); err != nil {
			return err
		}
		out.UserID, out.APIKeyID, out.AccountID = user.ID, key.ID, account.ID
	case "status", "status-multi":
		fixtureStage = "observe-usage-billing-slots"
		if in.UserID <= 0 || in.APIKeyID <= 0 || in.AccountID <= 0 {
			return errors.New("fixture ids required")
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
			FROM usage_logs WHERE user_id=$1 AND api_key_id=$2 AND account_id=$3`,
			in.UserID, in.APIKeyID, in.AccountID).Scan(&out.UsageRows, &out.InputTokens, &out.OutputTokens); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, in.APIKeyID).Scan(&out.DedupRows); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1`, in.UserID).Scan(&out.Balance); err != nil {
			return err
		}
		slots := repository.NewConcurrencyCache(cache, 15, 30)
		out.UserSlots, err = slots.GetUserConcurrency(ctx, in.UserID)
		if err != nil {
			return err
		}
		out.AccountSlots, err = slots.GetAccountConcurrency(ctx, in.AccountID)
		if err != nil {
			return err
		}
		keys, err := slots.(service.APIKeyConcurrencyCache).GetAPIKeyConcurrencyBatch(ctx, []int64{in.APIKeyID})
		if err != nil {
			return err
		}
		out.APIKeySlots = keys[in.APIKeyID]
		out.APIKeyQueueActive, out.APIKeyQueueWaiting, err = slots.(service.APIKeySlotQueueCache).GetAPIKeyQueueStats(ctx, in.APIKeyID)
		if err != nil {
			return err
		}
		if op == "status-multi" {
			if in.PeerAccountID <= 0 || in.PeerAccountID == in.AccountID {
				return errors.New("distinct fixture peer account required")
			}
			if err := db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
				FROM usage_logs WHERE user_id=$1 AND api_key_id=$2 AND account_id=$3`,
				in.UserID, in.APIKeyID, in.PeerAccountID).Scan(&out.PeerUsageRows, &out.PeerInputTokens, &out.PeerOutputTokens); err != nil {
				return err
			}
			out.PeerAccountSlots, err = slots.GetAccountConcurrency(ctx, in.PeerAccountID)
			if err != nil {
				return err
			}
		}
	default:
		return errors.New("unknown fixture operation")
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

type modelState struct {
	mu                                    sync.Mutex
	accepted, completed, active           int
	adminRequests                         int
	release                               chan struct{}
	releaseOnce                           sync.Once
	failureRelease                        chan struct{}
	failureReleaseOnce                    sync.Once
	primaryAccepted, peerAccepted         int
	primaryCompleted, peerCompleted       int
	primaryHTTPAttempts, peerHTTPAttempts int
}

func serve() error {
	s := &modelState{release: make(chan struct{}), failureRelease: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.adminRequests++
		s.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-upstream-only" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-6.1-sol","object":"model","owned_by":"fixture-pool"}]}`)
	})
	mux.HandleFunc("/fixture/release", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.releaseOnce.Do(func() { close(s.release) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/fixture/release-failure", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.failureReleaseOnce.Do(func() { close(s.failureRelease) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/fixture/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": s.accepted, "completed": s.completed, "active": s.active, "admin_requests": s.adminRequests})
	})
	mux.HandleFunc("/fixture/multi-stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"primary_accepted": s.primaryAccepted,
			"peer_accepted": s.peerAccepted, "primary_completed": s.primaryCompleted, "peer_completed": s.peerCompleted,
			"primary_http_attempts": s.primaryHTTPAttempts, "peer_http_attempts": s.peerHTTPAttempts})
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		primary := r.Header.Get("Authorization") == "Bearer synthetic-upstream-only"
		peer := r.Header.Get("Authorization") == "Bearer synthetic-upstream-peer"
		if r.Method == http.MethodGet && primary && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			serveCPRWebSocket(w, r, s)
			return
		}
		// Observed HTTP arrivals, before body parsing/acceptance. These do not
		// count application attempts that fail DNS/TCP/TLS before reaching us.
		s.mu.Lock()
		if primary {
			s.primaryHTTPAttempts++
		} else if peer {
			s.peerHTTPAttempts++
		}
		s.mu.Unlock()
		var body struct {
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body) != nil ||
			body.Model != "gpt-6.1-sol" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		multiAcceptedEOF := bytes.Contains(body.Input, []byte("fixture-multi-account-accepted-eof"))
		multiPartialEOF := bytes.Contains(body.Input, []byte("fixture-multi-account-partial-eof"))
		multi := multiAcceptedEOF || multiPartialEOF
		if !primary && !(multi && peer) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.accepted++
		id := s.accepted
		s.active++
		firstMulti := false
		if multi {
			firstMulti = s.primaryAccepted+s.peerAccepted == 0
			if peer {
				s.peerAccepted++
			} else {
				s.primaryAccepted++
			}
		}
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
		if (multiAcceptedEOF && firstMulti) || bytes.Contains(body.Input, []byte("fixture-accepted-before-response-eof")) {
			// Body is fully read, authenticated and counted, but no response has
			// been written. This is distinct from EOF after semantic output.
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", fmt.Sprintf("fixture-model-%d", id))
		event := func(name string, payload any) error {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
			w.(http.Flusher).Flush()
			return err
		}
		responseID := fmt.Sprintf("resp_fixture_%d", id)
		response := map[string]any{"id": responseID, "object": "response", "created_at": 1700000000,
			"model": "gpt-6.1-sol", "status": "in_progress", "output": []any{}}
		if event("response.created", map[string]any{"type": "response.created", "response": response}) != nil {
			return
		}
		if event("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "OK"}) != nil {
			return
		}
		if multiPartialEOF || bytes.Contains(body.Input, []byte("fixture-partial-sse-eof")) {
			// Deliberately finish the HTTP response after real semantic output,
			// without terminal usage/completion. The application must not turn
			// this accepted/partly emitted request into another generation.
			return
		}
		if bytes.Contains(body.Input, []byte("fixture-held-partial-sse-eof")) {
			// The failed stream has its own control so its peer's successful
			// stream stays active while real per-request leases are released.
			select {
			case <-s.failureRelease:
			case <-r.Context().Done():
				return
			case <-time.After(12 * time.Second):
				return
			}
			return // Semantic output, but no terminal usage or completion.
		}
		if bytes.Contains(body.Input, []byte("fixture-hold-until-release")) {
			// Synthetic control only, not a gateway replay/recovery mechanism.
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			case <-time.After(12 * time.Second):
				return
			}
		}
		response["status"] = "completed"
		response["output"] = []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}}
		response["usage"] = map[string]any{"input_tokens": 7, "output_tokens": 2, "total_tokens": 9}
		if event("response.completed", map[string]any{"type": "response.completed", "response": response}) != nil {
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		s.mu.Lock()
		s.completed++
		if multi {
			if peer {
				s.peerCompleted++
			} else {
				s.primaryCompleted++
			}
		}
		s.mu.Unlock()
	})
	server := &http.Server{Addr: ":9090", Handler: mux, ReadHeaderTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func cprFixtureWSFrames() []string {
	return []string{
		` { "type":"response.create","model":"gpt-6.1-sol","store":false,"instructions":"Synthetic fixture only.","input":"one","client_marker":"unmodified" } `,
		`{"type":"response.create","model":"gpt-6.1-sol","store":false,"previous_response_id":"resp_fixture_ws_1","input":"two","client_marker":"unmodified"}`,
	}
}

func serveCPRWebSocket(w http.ResponseWriter, r *http.Request, s *modelState) {
	conn, err := coderws.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	for turn, expected := range cprFixtureWSFrames() {
		kind, body, err := conn.Read(ctx)
		if err != nil || kind != coderws.MessageText || string(body) != expected {
			return
		}
		s.mu.Lock()
		s.accepted++
		s.active++
		s.mu.Unlock()
		id := fmt.Sprintf("resp_fixture_ws_%d", turn+1)
		frames := []string{
			fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":"gpt-6.1-sol"}}`, id),
			fmt.Sprintf(`{"type":"response.output_text.delta","response_id":%q,"delta":"OK"}`, id),
			fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-6.1-sol","status":"completed","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`, id),
		}
		for _, frame := range frames {
			if err := conn.Write(ctx, coderws.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		s.mu.Lock()
		s.completed++
		s.active--
		s.mu.Unlock()
	}
	_, _, _ = conn.Read(ctx)
}

func probeCPRWebSocket(ctx context.Context, prefix, key string) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	conn, _, err := coderws.Dial(ctx, "ws://"+prefix+"-app:8080/v1/responses", &coderws.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
		HTTPHeader: http.Header{"Authorization": {"Bearer " + key}},
	})
	if err != nil {
		return errors.New("fixture websocket connection failed")
	}
	defer conn.CloseNow()
	for _, frame := range cprFixtureWSFrames() {
		if err := conn.Write(ctx, coderws.MessageText, []byte(frame)); err != nil {
			return errors.New("fixture websocket write failed")
		}
		for {
			_, body, err := conn.Read(ctx)
			if err != nil {
				return errors.New("fixture websocket read failed")
			}
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(body, &event) != nil || event.Type == "error" || event.Type == "response.failed" {
				return errors.New("fixture websocket protocol failure")
			}
			if event.Type == "response.completed" {
				break
			}
		}
	}
	return conn.Close(coderws.StatusNormalClosure, "fixture completed")
}
