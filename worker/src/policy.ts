// Server-side policy evaluation (docs/ARCHITECTURE.md D3). The decision is
// made by internal/policy, the same Go code the CLI previews with, compiled
// to a WASI reactor module by `make wasm`. This file is only the loader and a
// minimal WASI shim: the module does no I/O, so every WASI call it could make
// is either a no-op, a clock or entropy read, or a hard failure.

import policyModule from "./wasm/policy.wasm";
import type { Action } from "./project";

/** Must match policy.WireVersion: v4 binary guards; v5 reviewer.kind and ai. */
export const WIRE_VERSION = 5;

export interface WireChange {
  path: string;
  added_lines: string[];
  removed_count: number;
  /** The new side cannot be read as text; the detectors saw none of it. A deleted binary is not binary. */
  binary?: boolean;
  /** Size of the new side, when known. */
  bytes?: number;
}

export interface WireRequest {
  policy_yaml: string;
  changes: WireChange[];
  jurisdiction: string;
  truncated?: boolean;
}

export interface WireMatch {
  id: string;
  action: Action;
  approvers?: number;
  reason: string;
}

/** A rule in `mode: shadow` that matched. It never decides; it is logged. */
export interface ShadowMatch {
  rule: string;
  index: number;
  action: Action;
  approvers?: number;
  reason: string;
  /** The rule comes before the deciding rule, so enforcing it would have decided. */
  preempts: boolean;
}

export interface Evidence {
  tag: string;
  value: string;
  path?: string;
  line?: number;
  pattern?: string;
  detail?: string;
}

export interface Tags {
  areas: string[];
  data_classes: string[];
  risk: string;
  jurisdiction: string;
  lines_changed: number;
  files_changed: number;
  /** Binary files, in change order: the content detectors read none of them. Absent on decisions recorded before wire v4. */
  unscanned?: string[];
  /** Total size of the binary files' new sides, as far as it was known. */
  binary_bytes?: number;
  evidence: Evidence[];
}

/** A rule's `reviewer` (policy.Reviewer in Go). */
export interface Reviewer {
  different_family?: boolean;
  /** Who reviews an agent_review change: any (default), ai or agent. */
  kind?: "any" | "ai" | "agent";
}

/** A rule's `ai` block (policy.AI in Go). */
export interface AiSettings {
  /** Below this the AI's approval is not trusted (0 or absent: the Worker's default). */
  min_confidence?: number;
  /** A pinned reviewer model; used only when its family differs from the author's and its provider is enabled. */
  model?: string;
  /** Write AI advice on propose for a change routed to a person. */
  advise?: boolean;
}

export interface ServerDecision {
  v: number;
  tags: Tags;
  matched_rule: string;
  action: Action;
  approvers: number;
  /** Who may approve; absent when anyone but the author may. Enforced in decide (different_family) and aireview (kind). */
  reviewer?: Reviewer;
  /** The rule's AI settings; absent when it says nothing. Read by aireview and aiadvice. */
  ai?: AiSettings;
  reason: string;
  default: boolean;
  shadow_matches: ShadowMatch[];
  /** Enforcing the first preempting shadow rule would have changed the action. */
  would_differ: boolean;
  overrode?: WireMatch;
  policy_error?: string;
}

interface WireResponse extends Partial<ServerDecision> {
  v: number;
  error?: { code: string; message: string };
}

export class PolicyEngineError extends Error {}

interface Exports {
  memory: WebAssembly.Memory;
  _initialize(): void;
  wg_alloc(n: number): number;
  wg_evaluate(n: number): bigint;
  wg_wire_version(): number;
}

class WasiExit extends Error {
  constructor(readonly code: number) {
    super(`policy wasm exited with code ${code}`);
  }
}

const ERRNO_SUCCESS = 0;
const ERRNO_BADF = 8;

function wasiImports(memory: () => WebAssembly.Memory): Record<string, (...args: any[]) => number> {
  const view = () => new DataView(memory().buffer);
  const decoder = new TextDecoder();
  let stderr = "";
  return {
    args_sizes_get: (argc: number, size: number) => (view().setUint32(argc, 0, true), view().setUint32(size, 0, true), ERRNO_SUCCESS),
    args_get: () => ERRNO_SUCCESS,
    environ_sizes_get: (n: number, size: number) => (view().setUint32(n, 0, true), view().setUint32(size, 0, true), ERRNO_SUCCESS),
    environ_get: () => ERRNO_SUCCESS,
    clock_time_get: (_id: number, _precision: bigint, out: number) => {
      view().setBigUint64(out, BigInt(Date.now()) * 1_000_000n, true);
      return ERRNO_SUCCESS;
    },
    random_get: (ptr: number, len: number) => {
      const bytes = new Uint8Array(memory().buffer, ptr, len);
      for (let i = 0; i < len; i += 65536) crypto.getRandomValues(bytes.subarray(i, Math.min(i + 65536, len)));
      return ERRNO_SUCCESS;
    },
    // Only stderr is expected (a Go panic message); surface it in the logs.
    fd_write: (fd: number, iovs: number, iovsLen: number, nwritten: number) => {
      const v = view();
      let n = 0;
      for (let i = 0; i < iovsLen; i++) {
        const ptr = v.getUint32(iovs + i * 8, true);
        const len = v.getUint32(iovs + i * 8 + 4, true);
        if (fd === 1 || fd === 2) stderr += decoder.decode(new Uint8Array(memory().buffer, ptr, len));
        n += len;
      }
      v.setUint32(nwritten, n, true);
      if (stderr.includes("\n")) {
        console.error("policy wasm:", stderr.trimEnd());
        stderr = "";
      }
      return ERRNO_SUCCESS;
    },
    fd_read: () => ERRNO_BADF,
    fd_close: () => ERRNO_BADF,
    fd_fdstat_get: (fd: number, out: number) => {
      if (fd > 2) return ERRNO_BADF;
      const v = view();
      for (let i = 0; i < 24; i++) v.setUint8(out + i, 0);
      v.setUint8(out, 2); // filetype: character device
      return ERRNO_SUCCESS;
    },
    fd_fdstat_set_flags: () => ERRNO_SUCCESS,
    fd_prestat_get: () => ERRNO_BADF, // no preopened directories
    fd_prestat_dir_name: () => ERRNO_BADF,
    sched_yield: () => ERRNO_SUCCESS,
    // The evaluator never sleeps or waits; if it tries, fail loudly rather than spin.
    poll_oneoff: () => {
      throw new PolicyEngineError("policy wasm tried to block (poll_oneoff)");
    },
    proc_exit: (code: number) => {
      throw new WasiExit(code);
    },
  };
}

let instance: Promise<Exports> | null = null;

async function load(): Promise<Exports> {
  let exports: Exports | null = null;
  const inst = await WebAssembly.instantiate(policyModule, {
    wasi_snapshot_preview1: wasiImports(() => exports!.memory),
  });
  exports = inst.exports as unknown as Exports;
  exports._initialize();
  const v = exports.wg_wire_version();
  if (v !== WIRE_VERSION) throw new PolicyEngineError(`policy wasm speaks wire v${v}, Worker expects v${WIRE_VERSION}; run make wasm`);
  return exports;
}

/**
 * Classifies a change and decides on it with the Go evaluator. One instance
 * per isolate; calls are synchronous inside, so they never interleave. A trap
 * discards the instance so the next call starts clean.
 */
export async function evaluatePolicy(req: WireRequest): Promise<ServerDecision> {
  instance ??= load();
  let ex: Exports;
  try {
    ex = await instance;
  } catch (err) {
    instance = null;
    throw err;
  }
  let raw: string;
  try {
    const input = new TextEncoder().encode(JSON.stringify(req));
    const ptr = ex.wg_alloc(input.length);
    new Uint8Array(ex.memory.buffer, ptr, input.length).set(input);
    const r = ex.wg_evaluate(input.length);
    const outPtr = Number(r >> 32n);
    const outLen = Number(r & 0xffffffffn);
    if (outLen === 0) throw new PolicyEngineError("policy wasm returned nothing");
    raw = new TextDecoder().decode(new Uint8Array(ex.memory.buffer, outPtr, outLen));
  } catch (err) {
    instance = null;
    if (err instanceof PolicyEngineError) throw err;
    throw new PolicyEngineError(`policy wasm failed: ${err instanceof Error ? err.message : String(err)}`);
  }
  const resp = JSON.parse(raw) as WireResponse;
  if (resp.error) throw new PolicyEngineError(`policy wasm rejected the request: ${resp.error.message}`);
  if (resp.v !== WIRE_VERSION || !resp.action) throw new PolicyEngineError("policy wasm returned an unexpected response");
  return resp as ServerDecision;
}
