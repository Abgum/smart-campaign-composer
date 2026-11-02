// Shapes of the backend's JSON (internal/httpapi, turns, tracking).

export interface LLMInfo {
  provider: 'anthropic' | 'mock' | string
  model: string
  reason?: string
}

export interface MockScenario {
  name: string
  description: string
}

export interface Meta {
  llm: LLMInfo
  router?: LLMInfo
  mock_scenarios?: MockScenario[]
}

export interface Message {
  turn_id: string
  role: 'user' | 'assistant'
  text: string
  outcome: 'ok' | 'partial' | 'failed' | null
  llm_provider: string | null
  llm_model: string | null
  created_at: string
}

export interface CampaignSummary {
  id: string
  ordinal: number
  name: string
  status: CampaignStatus
  version: number | null
  updated_at: string
}

export type CampaignStatus = 'draft' | 'approved' | 'sending' | 'completed' | 'cancelled'

export interface ChatSummary {
  id: string
  title: string
  updated_at: string
}

export interface Chat {
  id: string
  messages: Message[]
  campaigns: CampaignSummary[]
  running_turn_id: string | null
}

export interface TargetRef {
  campaign_id: string
  ordinal: number
  name: string
}

export interface RouteView {
  intent: 'create' | 'edit' | 'clarify' | 'chat'
  source: string
  allow_create: boolean
  targets: TargetRef[]
  overflow?: TargetRef[]
  references?: TargetRef[]
  candidates?: TargetRef[]
  question?: string
  reason?: string
  reject_reason?: string
}

export interface TurnResult {
  turn_id: string
  status: 'running' | 'completed' | 'failed'
  outcome?: 'ok' | 'partial' | 'failed'
  error_code?: string
  assistant_message?: {
    text: string
    outcome: string
    llm_provider: string
    llm_model: string
  }
  campaign_refs: {
    campaign_id: string
    version_id: number
    version: number
    ordinal: number
    action: string
  }[]
  route?: RouteView
}

export interface PlaceRef {
  level: 'country' | 'admin1' | 'city'
  name?: string
  code?: string
  geoname_id?: number
  country?: string
}

export interface Condition {
  kind: 'attribute' | 'location' | 'event'
  field?: string
  operator?: string
  values?: string[]
  places?: PlaceRef[]
  event_type?: string
  within_days?: number
  min_count?: number
  negate?: boolean
  not_followed_by?: string
}

export interface SegmentRule {
  match: 'all' | 'any'
  conditions: Condition[]
}

export interface RuleCatalog {
  fields: { name: string; type: 'int' | 'decimal' | 'language'; operators: string[]; min?: number; max?: number; description: string }[]
  events: { name: string; description: string; not_followed_by: string[] }[]
  languages: string[]
  places: PlaceRef[]
  max_conditions: number
  max_within_days: number
  max_min_count: number
  max_list_values: number
}

export interface ManualDraftInput {
  name: string
  segment_rule: SegmentRule
  sms: SMS | null
  email: Email | null
  // Omitted: the base version's language setup is kept.
  localization?: Localization | null
  // Omitted: the base version's A/B test is kept; null removes it.
  ab_test?: ABTest | null
}

// An A/B test's B side: one B per A content (the version's own content), in
// its language and on its channels. Each recipient gets A or B, the same arm
// on SMS and email.
export interface ABTest {
  variants: LanguageVariant[]
}

// Counts per A/B arm, then per channel.
export type VariantCounts = Record<string, Record<string, ChannelCounts>>

export interface ManualDraftResult {
  campaign_id: string
  chat_id?: string
  ordinal?: number
  version: number
  version_id: number
  created?: boolean
  change_summary?: string
}

export interface Audience {
  total: number
  sms_reachable: number
  email_reachable: number
}

export interface SMS {
  text: string
}

export interface Email {
  subject: string
  body: string
}

export interface Version {
  id: number
  version: number
  source: 'llm' | 'user' | string
  llm_provider: string | null
  name: string
  segment_rule: SegmentRule
  sms?: SMS | null
  email?: Email | null
  audience?: Audience | null
  change_summary: string
  created_at: string
  // The language setup (none for a version from before there were
  // languages) and how the audience split by language when it was saved.
  localization?: Localization | null
  language_summary?: LanguageSummary | null
  ab_test?: ABTest | null // the B side of an A/B test
}

// One content for everyone in a language, or one variant per language with
// each recipient getting theirs (language.Settings).
export interface Localization {
  mode: 'single' | 'per_recipient'
  language?: string
  variants?: LanguageVariant[]
  country_languages?: CountryLanguage[]
  decisions?: LanguageDecision[]
}

export interface LanguageVariant {
  language: string
  sms?: SMS | null
  email?: Email | null
}

export interface CountryLanguage {
  country: string
  language: string
}

// A waiting group (preference:<code>, country:<CC>, variant:<code>) assigned
// to a variant or left out.
export interface LanguageDecision {
  group: string
  action: 'assign' | 'exclude'
  language?: string
}

export interface GroupCount {
  group: string
  count: number
}

export interface LanguageReach {
  total: number
  sms: number
  email: number
}

export interface LanguageSummary {
  mode: string
  languages: Record<string, LanguageReach>
  unresolved?: GroupCount[] | null
  excluded?: GroupCount[] | null
  // Per country, the recipients whose language the country mapping decides
  // (no supported preference).
  countries?: Record<string, number> | null
}

// Counts per language, then per channel.
export type LanguageCounts = Record<string, Record<string, ChannelCounts>>

// What approving a version would do now (nothing is written).
export interface ApprovalPreview {
  version_id: number
  mode?: string
  channels: Record<string, ChannelCounts>
  languages: LanguageCounts
  unresolved: GroupCount[] | null
  excluded_language: GroupCount[] | null
  country_languages: CountryLanguage[] | null
  decisions: LanguageDecision[] | null
  samples: ApprovalSample[] | null
  issues: Issue[] | null
  variants?: VariantCounts | null // per A/B arm
}

export interface ApprovalSample {
  language: string
  channel: string
  variant?: string // the A/B arm
  subject?: string | null
  body: string
}

export interface VersionEntry {
  id: number
  version: number
  source: string
  llm_provider: string | null
  change_summary: string
  created_at: string
}

export interface ChannelCounts {
  selected: number
  excluded_consent: number
  excluded_duplicate: number
}

export interface CampaignDetail {
  id: string
  chat_id: string
  ordinal: number
  name: string
  status: CampaignStatus
  current_version: Version | null
  approved_version_id: number | null
  approval: Record<string, ChannelCounts> | null // the approval snapshot, per channel
  approval_languages?: { languages: LanguageCounts; excluded_language?: GroupCount[] | null } | null
  versions: VersionEntry[]
  updated_at: string
  scheduled_at?: string | null // when sending starts (UTC); null: at approval
}

export interface ApproveResult {
  campaign_id: string
  version_id: number
  status: string
  already_approved: boolean
  channels: Record<string, ChannelCounts>
  languages?: LanguageCounts | null
  excluded_language?: GroupCount[] | null
  scheduled_at?: string | null
  variants?: VariantCounts | null // per A/B arm
}

// What the marketer chooses when approving: a later sending time (UTC).
export interface ApproveOptions {
  scheduled_at?: string
}

export interface Issue {
  path: string
  code: string
  message: string
}

// Delivery counts of a campaign and channel (tracking.Counts).
export interface Counts {
  excluded_consent: number
  excluded_duplicate: number
  pending: number
  in_flight: number
  retry: number
  sent: number
  failed: number
  unknown: number
  skipped_no_consent: number
}

export interface TrackedCampaign {
  id: string
  chat_id: string
  ordinal: number
  name: string
  status: CampaignStatus
  updated_at: string
  approved_at: string | null
  scheduled_at?: string | null // when sending starts (UTC); null: at approval
  last_sent_at: string | null
  version: number | null // the approved version, else the current one
  llm_provider: string | null // the model that wrote that version
  channels: Record<string, Counts>
  variants?: Record<string, Record<string, Counts>> | null // an A/B campaign's numbers per arm, then channel
}

// GET /api/analytics (backend internal/analytics).
export interface ChannelTotals {
  total: number
  sms: number
  email: number
}

export interface SeriesPoint {
  at: string // 2026-10-27, or 2026-10-27T14:00 by hour
  sms: number
  email: number
}

export interface CityCount {
  geoname_id: number
  name: string // empty when the catalog does not know the id
  country?: string
  admin1?: string
  sms: number
  email: number
  total: number
}

export interface AnalyticsCities {
  top: CityCount[]
  other: { cities: number; total: number }
  unknown: ChannelTotals
}

export interface Analytics {
  window: { range: string; from: string; to: string; tz: string; bucket: 'day' | 'hour' }
  kpis: {
    sent: ChannelTotals
    // scheduled: pending messages of campaigns whose time has not come; in
    // none of the other counts
    queued: { total: number; pending: number; in_flight: number; retry: number; sms: number; email: number; scheduled?: number }
    failed: ChannelTotals
    unknown: ChannelTotals
  }
  series: SeriesPoint[]
  cities: AnalyticsCities
}

export interface ChannelState {
  due: number
  calls?: number
  window_ms?: number
  paused_until: string | null
}

export interface Worker {
  name: string
  beat_at: string
  alive: boolean
}

export interface Dashboard {
  at: string
  campaigns: TrackedCampaign[]
  totals: Record<string, Counts>
  channels: Record<string, ChannelState>
  workers: Worker[]
  version?: string // changes whenever a delivery or campaign changes
}

// GET /api/requests: a message a marketer wrote and what came of it.
export interface RecentRequest {
  turn_id: string
  chat_id: string
  text: string
  status: 'running' | 'completed' | 'failed'
  outcome: '' | 'ok' | 'partial' | 'failed'
  intent: '' | 'create' | 'edit' | 'clarify' | 'chat'
  campaigns: { campaign_id: string; name: string; ordinal: number; version: number; action: string }[]
  llm_provider: string // who answered: 'anthropic' | 'mock'; '' while the turn runs
  created_at: string
}
