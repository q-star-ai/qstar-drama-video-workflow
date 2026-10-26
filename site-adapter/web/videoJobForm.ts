// Capability-driven storyboard video form helpers. Framework-agnostic: the
// site's existing shot editor calls these instead of hard-coding a provider.
// Only type annotations are used, so Node can strip them for tests.

export type InputMode = 'identity_refs' | 'first_last' | 'video_ref'

export type ModeLimits = {
  min_images: number
  max_images: number
  accepts_identity_images: boolean
  accepts_video: boolean
}

export type Capability = {
  provider: string
  model: string
  enabled: boolean
  modes: Partial<Record<InputMode, ModeLimits>>
  min_seconds: number
  max_seconds: number
  aspect_ratios: string[]
  verification_status: string
  fallback: boolean
  notes?: string
}

export type ShotDraft = {
  input_mode: InputMode
  duration_seconds: number
  aspect_ratio: string
  cast_count: number
  has_first_frame: boolean
  has_greybox: boolean
  previs_reviewed: boolean
}

export type ModeOption = {
  mode: InputMode
  label: string
  enabled: boolean
  // Shown next to a disabled option, or as a warning on an enabled one.
  note: string
  providers: string[]
}

const LABELS: Record<InputMode, string> = {
  identity_refs: '人物/服装/场景参考图',
  first_last: '预演首尾帧',
  video_ref: '灰盒参考片',
}

function imageCount(mode: InputMode, draft: ShotDraft): number {
  if (mode === 'first_last') return draft.has_first_frame ? 2 : 0
  return draft.cast_count * 2 + 1
}

/** Why a provider cannot run the draft in a mode, or '' when it can. */
export function rejection(cap: Capability, mode: InputMode, draft: ShotDraft): string {
  if (!cap.enabled) return '通道未启用'
  const limits = cap.modes[mode]
  if (!limits) {
    if (mode === 'video_ref') return `${cap.provider} 不支持灰盒视频输入`
    return `${cap.provider} 不支持${LABELS[mode]}`
  }
  if (draft.duration_seconds < cap.min_seconds || draft.duration_seconds > cap.max_seconds) {
    return `时长需在 ${cap.min_seconds}–${cap.max_seconds} 秒`
  }
  if (!cap.aspect_ratios.includes(draft.aspect_ratio)) return `不支持 ${draft.aspect_ratio}`
  const n = imageCount(mode, draft)
  if (mode !== 'first_last' && n > limits.max_images) return `参考图 ${n} 张，超过 ${limits.max_images} 张`
  return ''
}

/**
 * Mode choices for the shot editor. Server capabilities decide; the UI only
 * reflects them. A mode no enabled provider supports is shown disabled with
 * the reason, never silently routed elsewhere.
 */
export function modeOptions(caps: Capability[], draft: ShotDraft): ModeOption[] {
  return (Object.keys(LABELS) as InputMode[]).map((mode) => {
    const providers = caps.filter((c) => rejection(c, mode, { ...draft, input_mode: mode }) === '').map((c) => c.provider)
    const reasons = caps.map((c) => rejection(c, mode, { ...draft, input_mode: mode })).filter((r) => r !== '')
    let note = providers.length ? '' : reasons.join('；') || '没有可用通道'
    if (mode === 'first_last' && providers.length && draft.cast_count > 0) {
      note = '首尾帧模式不能同时提交人脸/服装参考图，成片需人工核对人物'
    }
    if (mode !== 'identity_refs' && !draft.previs_reviewed) {
      return { mode, label: LABELS[mode], enabled: false, note: '预演素材需先经人工审看', providers }
    }
    if (mode === 'first_last' && !draft.has_first_frame) {
      return { mode, label: LABELS[mode], enabled: false, note: '未登记预演首帧', providers }
    }
    if (mode === 'video_ref' && !draft.has_greybox) {
      return { mode, label: LABELS[mode], enabled: false, note: '未登记灰盒参考片', providers }
    }
    return { mode, label: LABELS[mode], enabled: providers.length > 0, note, providers }
  })
}

export type JobView = {
  id: string
  status: string
  status_reason?: string
  candidate_chain: Capability[]
  attempts: { provider: string; model: string; kind: string; external_task_id?: string; status: string; error_class?: string; cost_micro_usd: number }[]
  fallbacks: { from: string; to: string; class: string; reason: string }[]
  output?: { duration_seconds: number; width: number; height: number; has_audio: boolean; probe_failures?: string[] }
  qc?: { passed: boolean; failures?: string[]; reviewer: string }
}

/** Status line for the shot card. "完成" only for a probed, QC-passed file. */
export function describeJob(job: JobView): string {
  const cur = job.attempts[job.attempts.length - 1]
  const where = cur ? `${cur.provider}/${cur.model}${cur.external_task_id ? ` 任务 ${cur.external_task_id}` : ''}` : '未提交'
  const cost = job.attempts.reduce((sum, a) => sum + a.cost_micro_usd, 0) / 1e6
  const switches = job.fallbacks.map((f) => `${f.from}→${f.to}（${f.class}）`).join('，')
  const parts = [where, `费用 $${cost.toFixed(2)}`]
  if (switches) parts.push(`切换 ${switches}`)
  switch (job.status) {
    case 'succeeded':
      return `完成 · ${job.output?.width}x${job.output?.height} ${job.output?.duration_seconds.toFixed(1)}s · 质检 ${job.qc?.reviewer} · ${parts.join(' · ')}`
    case 'qc_pending':
      return `待人工质检 · ${parts.join(' · ')}`
    case 'needs_reconcile':
      return `提交结果未知，需核对供应商后台，勿重复提交 · ${parts.join(' · ')}`
    case 'failed':
    case 'blocked':
      return `${job.status === 'failed' ? '失败' : '受阻'}：${job.status_reason ?? ''} · ${parts.join(' · ')}`
    default:
      return `生成中 · ${parts.join(' · ')}`
  }
}

/** Stable per-shot-version key so a double click never pays twice. */
export function idempotencyKey(episodeId: string, shotId: string, shotRevision: number, inputMode: InputMode): string {
  return `${episodeId}/${shotId}/r${shotRevision}/${inputMode}`
}

export async function createJob(base: string, shot: unknown, key: string, fetchImpl: typeof fetch = fetch): Promise<JobView> {
  const res = await fetchImpl(`${base}/jobs`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key },
    body: JSON.stringify(shot),
    credentials: 'same-origin',
  })
  if (!res.ok) throw new Error((await res.json()).error ?? `HTTP ${res.status}`)
  return res.json()
}
