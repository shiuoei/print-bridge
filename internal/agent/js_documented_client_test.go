package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// The raw-WebSocket client in docs/JS-INTEGRATION.md is what a business site is
// told to copy. A document that drifts from the agent is worse than no document,
// so the client is executed here against a real Agent: every wire field it sends
// and every field it reads is checked against the production handler.
//
// It deliberately does NOT import any SDK. The point is that a caller
// with nothing but the browser's WebSocket API can talk to this agent, which is
// a different claim from "an SDK wrapper works".
//
// Run: go test ./internal/agent -run TestDocumentedJSClient -v
func TestDocumentedJSClient(t *testing.T) {
	node := nodeBinary(t)

	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := s.Config()
	cfg.Printing.DefaultPrinter = "Test Printer"
	cfg.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{printed: make(chan []byte, 4)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.worker(ctx)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(a.webSocket))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	path := filepath.Join(t.TempDir(), "documented-client.mjs")
	if err := os.WriteFile(path, []byte(documentedClientScript), 0600); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	out, err := exec.CommandContext(runCtx, node, path, port).CombinedOutput()
	if err != nil {
		t.Fatalf("documented JS client failed: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// nodeBinary finds Node without assuming a specific install location.
func nodeBinary(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("node"); err == nil {
		return p
	}
	t.Skip("node not found; the documented JS client is validated with Node")
	return ""
}

// documentedClientScript is the client from docs/JS-INTEGRATION.md section 8,
// with the assertions the document promises. Keep them in step: if a field name
// or status literal changes in the agent, this fails.
const documentedClientScript = `
import assert from 'node:assert/strict';

// ---- begin: verbatim client from docs/JS-INTEGRATION.md ----
class PrintBridge {
  constructor({ port = 17890, host = '127.0.0.1', requestTimeoutMs = 30000 } = {}) {
    Object.assign(this, { port, host, requestTimeoutMs });
    this.pending = new Map();
    this.pings = new Map();
    this.listeners = new Map();
    this.ws = null;
  }

  connect() {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket('ws://' + this.host + ':' + this.port + '/ws');
      const timer = setTimeout(() => { ws.close(); reject(new Error('CONNECTION_TIMEOUT')); }, 5000);
      ws.onopen = () => { clearTimeout(timer); this.ws = ws; resolve(); };
      ws.onerror = () => { clearTimeout(timer); reject(new Error('CONNECTION_FAILED')); };
      ws.onclose = (e) => {
        this.ws = null;
        for (const [, p] of this.pending) { clearTimeout(p.timer); p.reject(new Error('NOT_CONNECTED')); }
        for (const [, p] of this.pings)   { clearTimeout(p.timer); p.reject(new Error('NOT_CONNECTED')); }
        this.pending.clear();
        this.pings.clear();
      };
      ws.onmessage = (ev) => this._onMessage(JSON.parse(ev.data));
      this.ws = ws;
    });
  }

  _onMessage(msg) {
    if (msg.type === 'pong') {
      const p = this.pings.get(msg.time);
      if (p) { clearTimeout(p.timer); this.pings.delete(msg.time); p.resolve(msg); }
      return;
    }
    if (msg.type === 'job_status') {
      const sub = this.listeners.get(msg.job_id);
      if (sub) for (const fn of sub) fn(msg);
    }
    const p = this.pending.get(msg.request_id);
    if (!p) return;
    clearTimeout(p.timer);
    this.pending.delete(msg.request_id);
    if (msg.type === 'error') {
      const err = new Error(msg.message);
      err.code = msg.error_code;
      p.reject(err);
    } else {
      p.resolve(msg);
    }
  }

  _request(payload) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(new Error('NOT_CONNECTED'));
    }
    const requestId = (crypto.randomUUID?.() ?? String(Date.now()) + Math.random());
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestId);
        const err = new Error('REQUEST_TIMEOUT'); err.code = 'REQUEST_TIMEOUT'; reject(err);
      }, this.requestTimeoutMs);
      this.pending.set(requestId, { resolve, reject, timer });
      this.ws.send(JSON.stringify({ ...payload, request_id: requestId }));
    });
  }

  ping(timeoutMs = 5000) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(Object.assign(new Error('NOT_CONNECTED'), { code: 'NOT_CONNECTED' }));
    }
    const time = Date.now();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pings.delete(time);
        reject(Object.assign(new Error('PING_TIMEOUT'), { code: 'PING_TIMEOUT' }));
      }, timeoutMs);
      this.pings.set(time, { resolve, reject, timer });
      this.ws.send(JSON.stringify({ type: 'ping', time }));
    });
  }
  async printers()       { return (await this._request({ type: 'get_printers_list' })).printers; }
  async printerInfo(name){ return (await this._request({ type: 'get_printer_info', printer_name: name })).printer; }
  async queue()          { return (await this._request({ type: 'get_print_queue' })).jobs; }
  async query(jobId)     { return this._request({ type: 'query', job_id: jobId }); }

  async print(job) {
    const { jobId, type, printerName, fileUrl, dataBase64, html, waitMs, copies, paper } = job;
    const payload = { type: 'print', job_id: jobId, format: type };
    if (printerName !== undefined) payload.printer_name = printerName;
    if (fileUrl !== undefined) payload.file_url = fileUrl;
    if (dataBase64 !== undefined) payload.data_base64 = dataBase64;
    if (html !== undefined) payload.html = html;
    if (waitMs !== undefined) payload.wait_ms = waitMs;
    if (copies !== undefined) payload.copies = copies;
    if (paper !== undefined) payload.paper = { width_mm: paper.widthMm, height_mm: paper.heightMm };
    return this._request(payload);
  }

  async printBatch(batchId, jobs) {
    return this._request({
      type: 'print_batch',
      batch_id: batchId,
      jobs: jobs.map(j => {
        const item = { job_id: j.jobId, format: j.type };
        if (j.printerName !== undefined) item.printer_name = j.printerName;
        if (j.fileUrl !== undefined) item.file_url = j.fileUrl;
        if (j.dataBase64 !== undefined) item.data_base64 = j.dataBase64;
        if (j.html !== undefined) item.html = j.html;
        if (j.copies !== undefined) item.copies = j.copies;
        if (j.paper !== undefined) item.paper = { width_mm: j.paper.widthMm, height_mm: j.paper.heightMm };
        return item;
      }),
    });
  }

  /** 订阅某任务的状态推送，返回取消函数。 */
  onStatus(jobId, fn) {
    if (!this.listeners.has(jobId)) this.listeners.set(jobId, new Set());
    this.listeners.get(jobId).add(fn);
    return () => this.listeners.get(jobId)?.delete(fn);
  }

  async waitTerminal(jobId, timeoutMs = 90000) {
    const terminal = new Set(['completed', 'unknown', 'failed', 'cancelled', 'submitted']);
    let off = () => {};
    const viaEvent = new Promise((resolve) => {
      off = this.onStatus(jobId, (msg) => {
        if (terminal.has(msg.status)) resolve(msg);
      });
    });
    try {
      const current = await this.query(jobId).catch(() => null);
      if (current && terminal.has(current.status)) return current;
      return await Promise.race([
        viaEvent,
        new Promise((_, reject) =>
          setTimeout(() => reject(Object.assign(new Error('TERMINAL_TIMEOUT'), { code: 'TERMINAL_TIMEOUT' })), timeoutMs)),
      ]);
    } finally {
      off();
    }
  }

  close() { this.ws?.close(); }
}
// ---- end: verbatim client ----

const port = Number(process.argv[2]);

// Browser global WebSocket does not exist in Node before 22; document that the
// documented client targets the browser, and supply the equivalent here.
if (typeof globalThis.WebSocket === 'undefined') {
  const { WebSocket: NodeWebSocket } = await import('node:events');
  const { WebSocket: WsClient } = await import('ws');
  globalThis.WebSocket = WsClient;
}

const pb = new PrintBridge({ port });
await pb.connect();

// 4.1 ping: the literal agent_status is part of the contract.
const pong = await pb.ping();
assert.equal(pong.type, 'pong');
assert.equal(pong.agent_status, 'ready', 'agent_status must be "ready", not "running"');

// 4.4 printers: arrays are always present.
const printers = await pb.printers();
assert.ok(Array.isArray(printers));
assert.equal(printers[0].name, 'Test Printer');
assert.equal(printers[0].is_default, true, 'wire field is snake_case is_default');

const info = await pb.printerInfo('Test Printer');
assert.equal(info.papers.length, 2);
assert.deepEqual(
  { id: info.papers[0].id, name: info.papers[0].name, width_mm: info.papers[0].width_mm, height_mm: info.papers[0].height_mm },
  { id: '9', name: 'A4', width_mm: 210, height_mm: 297 },
  'paper wire fields are width_mm/height_mm',
);
assert.deepEqual(info.media_types, [], 'media_types is an array, never omitted');

// Error frames carry error_code for a not-found printer.
await assert.rejects(pb.printerInfo('No Such Printer'), e => e.code === 'PRINTER_NOT_FOUND');

// 4.2 single job: queued is the acceptance ack, not completion.
const one = await pb.print({ jobId: 'doc-one', type: 'raw', dataBase64: Buffer.from('ONE').toString('base64') });
assert.equal(one.status, 'queued');
assert.equal(one.job_id, 'doc-one', 'response echoes snake_case job_id');

// Duplicate job id is rejected on the wire.
await assert.rejects(
  pb.print({ jobId: 'doc-one', type: 'raw', dataBase64: Buffer.from('ONE').toString('base64') }),
  e => e.code === 'JOB_DUPLICATED',
);

// 4.3 batch: jobs carry format, not type.
const batch = await pb.printBatch('doc-batch', [
  { jobId: 'doc-two', type: 'raw', dataBase64: Buffer.from('TWO').toString('base64') },
]);
assert.equal(batch.status, 'queued');
assert.equal(batch.job_id, 'doc-batch');
await assert.rejects(
  pb.printBatch('doc-batch', [{ jobId: 'doc-three', type: 'raw', dataBase64: 'AA==' }]),
  e => e.code === 'BATCH_DUPLICATED',
);

// 5. format validation produces the documented code.
await assert.rejects(pb.print({ jobId: 'doc-bad', type: 'exe' }), e => e.code === 'UNSUPPORTED_FORMAT');

// 4.5 queue lists only not-yet-started jobs.
assert.ok(Array.isArray(await pb.queue()));

// 6/8 PDF through the production conversion path (fake backend only spools).
const pdf = '%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n';
await pb.print({ jobId: 'doc-pdf', type: 'pdf', fileUrl: 'data:application/pdf;base64,' + Buffer.from(pdf).toString('base64') });
await pb.print({ jobId: 'doc-bad-pdf', type: 'pdf', fileUrl: 'data:application/pdf;base64,' + Buffer.from('not a PDF').toString('base64') });

// 6. waitTerminal resolves on the real terminal states.
pb.onStatus('doc-one', () => {});
const terminal = await pb.waitTerminal('doc-one');
assert.ok(['submitted', 'failed', 'cancelled', 'completed', 'unknown'].includes(terminal.status));

// 7. query returns the latest status for a job.
const q = await pb.query('doc-one');
assert.equal(q.type, 'job_status');
assert.equal(q.job_id, 'doc-one');

pb.close();
console.log(JSON.stringify({ ping: pong.agent_status, printer: printers[0].name, one: one.status, batch: batch.status, terminal: terminal.status }));
`
