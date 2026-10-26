import { test } from 'node:test'
import assert from 'node:assert/strict'
import { modeOptions, describeJob, idempotencyKey, type Capability, type ShotDraft } from './videoJobForm.ts'

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
