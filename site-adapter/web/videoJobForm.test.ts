import { test } from 'node:test'
import assert from 'node:assert/strict'
import { modeOptions, describeJob, idempotencyKey, reserveAttempt, releaseAttempt, pendingAttempt, activeJobsFor, classifySubmitError, checkBeforeSubmit, newActiveSince, type Capability, type ShotDraft, type JobView } from './videoJobForm.ts'

const h3: Capability = {
  provider: 'minimax-h3', model: 'h3-profile', enabled: true, fallback: true, verification_status: 'candidate',
  modes: {
    identity_refs: { min_images: 1, max_images: 9, accepts_identity_images: true, accepts_video: false },
    first_last: { min_images: 1, max_images: 2, accepts_identity_images: false, accepts_video: false },
  },
  min_seconds: 2, max_seconds: 15, aspect_ratios: ['9:16', '16:9', '1:1'],
}

const draft: ShotDraft = {
  input_mode: 'identity_refs', duration_seconds: 8, aspect_ratio: '9:16', cast_count: 1,
  has_first_frame: true, has_greybox: true, previs_reviewed: true,
}

test('greybox is offered only when a provider declares video input', () => {
  const opts = modeOptions([h3], draft)
  const grey = opts.find((o) => o.mode === 'video_ref')!
  assert.equal(grey.enabled, false)
  assert.match(grey.note, /不支持灰盒视频输入/)
  assert.equal(opts.find((o) => o.mode === 'identity_refs')!.enabled, true)
})

test('first_last warns that faces are not carried', () => {
  const fl = modeOptions([h3], draft).find((o) => o.mode === 'first_last')!
  assert.equal(fl.enabled, true)
  assert.match(fl.note, /人工核对人物/)
})

test('previs must be reviewed and registered', () => {
  const opts = modeOptions([h3], { ...draft, previs_reviewed: false, has_first_frame: false })
  assert.equal(opts.find((o) => o.mode === 'first_last')!.enabled, false)
})

test('too many references disable identity mode', () => {
  const opts = modeOptions([h3], { ...draft, cast_count: 5 })
  const ids = opts.find((o) => o.mode === 'identity_refs')!
  assert.equal(ids.enabled, false)
  assert.match(ids.note, /超过 9 张/)
})

test('job card shows model, task, cost, fallback and never "完成" before QC', () => {
  const job = {
    id: 'j', status: 'qc_pending', candidate_chain: [h3],
    attempts: [
      { provider: 'doubao', model: 'x', kind: 'submit', external_task_id: 'db-1', status: 'failed', error_class: 'task_failed', cost_micro_usd: 0 },
      { provider: 'minimax-h3', model: 'h3-profile', kind: 'submit', external_task_id: 'prompt-abc', status: 'succeeded', cost_micro_usd: 420000 },
    ],
    fallbacks: [{ from: 'doubao', to: 'minimax-h3', class: 'task_failed', reason: 'async failed' }],
  }
  const line = describeJob(job)
  assert.match(line, /^待人工质检/)
  assert.match(line, /minimax-h3\/h3-profile 任务 prompt-abc/)
  assert.match(line, /\$0\.42/)
  assert.match(line, /doubao→minimax-h3/)
})

test('idempotency key is stable per shot revision and mode', () => {
  assert.equal(idempotencyKey('ep01', '01', 3, 'first_last'), 'ep01/01/r3/first_last')
})

test('uncertain submission reuses its key and a confirmed reroll gets a new key', () => {
  const values = new Map<string, string>()
  const storage = { getItem: (k: string) => values.get(k) ?? null, setItem: (k: string, v: string) => { values.set(k, v) }, removeItem: (k: string) => { values.delete(k) } }
  let n = 0
  const makeId = () => `attempt-${++n}`
  const first = reserveAttempt(storage, 'ep01/01/r3/first_last', makeId)
  assert.deepEqual(reserveAttempt(storage, 'ep01/01/r3/first_last', makeId), first)
  releaseAttempt(storage, first.marker, first.key)
  const next = reserveAttempt(storage, 'ep01/01/r3/first_last', makeId)
  assert.notEqual(next.key, first.key)
  assert.equal(n, 2)
})

function memoryStorage() {
  const m = new Map<string, string>()
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => { m.set(k, v) }, removeItem: (k: string) => { m.delete(k) } }
}

test('an unconfirmed submit keeps its key until a definite answer', () => {
  const store = memoryStorage()
  const base = idempotencyKey('ep1', '7', 3, 'identity_refs')
  assert.equal(pendingAttempt(store, base), null)
  const first = reserveAttempt(store, base, () => 'uuid-1')
  // Network failure: nothing released; the retry (even after reopening the tab) reuses the key.
  assert.equal(pendingAttempt(store, base), first.key)
  assert.equal(reserveAttempt(store, base, () => 'uuid-2').key, first.key)
  // A definite answer releases it; the next deliberate take gets a new key.
  releaseAttempt(store, first.marker, first.key)
  assert.equal(pendingAttempt(store, base), null)
  assert.notEqual(reserveAttempt(store, base, () => 'uuid-3').key, first.key)
})

test('only in-progress jobs of the same shot version and mode count as active', () => {
  const job = (status: string, rev: number, mode: string): JobView => ({ id: `${status}-${rev}-${mode}`, status, candidate_chain: [], attempts: [], fallbacks: [], shot_spec_revision: rev, request: { input_mode: mode } })
  const jobs = [job('running', 3, 'identity_refs'), job('probing', 3, 'identity_refs'), job('needs_reconcile', 3, 'identity_refs'),
    job('succeeded', 3, 'identity_refs'), job('failed', 3, 'identity_refs'), job('qc_pending', 3, 'identity_refs'),
    job('running', 2, 'identity_refs'), job('running', 3, 'first_last')]
  assert.deepEqual(activeJobsFor(jobs, 3, 'identity_refs').map((j) => j.status), ['running', 'probing', 'needs_reconcile'])
})

test('4xx answers are definite; network errors, timeouts and 5xx are not', () => {
  const definite = classifySubmitError({ isAxiosError: true, response: { status: 403, data: { error: { code: 'INSUFFICIENT_CREDITS', message: '积分不足' } } }, message: 'Request failed' })
  assert.deepEqual([definite.definitive, definite.code, definite.message], [true, 'INSUFFICIENT_CREDITS', '积分不足'])
  assert.equal(classifySubmitError({ isAxiosError: true, response: { status: 409, data: { error: { code: 'SHOT_CHANGED', message: '镜头参数已变化' } } } }).code, 'SHOT_CHANGED')
  assert.equal(classifySubmitError({ isAxiosError: true, response: { status: 502 } }).definitive, false)
  assert.equal(classifySubmitError({ isAxiosError: true, code: 'ECONNABORTED', message: 'timeout of 60000ms exceeded' }).definitive, false)
  assert.equal(classifySubmitError({ isAxiosError: true, code: 'ERR_NETWORK', message: 'Network Error' }).definitive, false)
  // The site interceptor turns a 200 with success:false into a plain Error.
  assert.equal(classifySubmitError(new Error('请求失败')).definitive, true)
})

test('submit decisions use a fresh server list; a failed refresh stops the submit', async () => {
  const base = idempotencyKey('ep1', '7', 3, 'identity_refs')
  const running = (id: string): JobView => ({ id, status: 'running', candidate_chain: [], attempts: [], fallbacks: [], shot_spec_revision: 3, request: { input_mode: 'identity_refs' } })
  let calls = 0
  // The tab's own list is empty (stale); the server already has a running take.
  const fresh = await checkBeforeSubmit(async () => { calls++; return [running('other-tab')] }, memoryStorage(), base, 3, 'identity_refs')
  assert.equal(calls, 1)
  assert.equal(fresh.kind, 'new')
  assert.equal(fresh.kind === 'new' && fresh.active.length, 1)

  const failed = await checkBeforeSubmit(async () => { throw new Error('Network Error') }, memoryStorage(), base, 3, 'identity_refs')
  assert.equal(failed.kind, 'blocked')
  assert.match(failed.kind === 'blocked' ? failed.message : '', /已停止提交/)
  const garbage = await checkBeforeSubmit(async () => ({}) as unknown as JobView[], memoryStorage(), base, 3, 'identity_refs')
  assert.equal(garbage.kind, 'blocked')

  // An unconfirmed earlier submit is re-queried with its key, never a new take.
  const store = memoryStorage()
  reserveAttempt(store, base, () => 'uuid-1')
  const requery = await checkBeforeSubmit(async () => [running('other-tab')], store, base, 3, 'identity_refs')
  assert.equal(requery.kind, 'requery')

  const broken = { getItem: () => { throw new Error('denied') }, setItem: () => {}, removeItem: () => {} }
  let listed = false
  const blocked = await checkBeforeSubmit(async () => { listed = true; return [] }, broken, base, 3, 'identity_refs')
  assert.deepEqual([blocked.kind, listed], ['blocked', false])
})

test('a take submitted while the dialog was open is detected', () => {
  const job = (id: string): JobView => ({ id, status: 'running', candidate_chain: [], attempts: [], fallbacks: [] })
  assert.deepEqual(newActiveSince([job('a')], [job('a'), job('b')]).map((j) => j.id), ['b'])
  assert.equal(newActiveSince([job('a'), job('b')], [job('b')]).length, 0)
})
