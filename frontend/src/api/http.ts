import { langHeader } from '../i18n'
import type { ApprovalPreview, ApproveOptions, ApproveResult, CampaignDetail, Chat, ChatSummary, Dashboard, ManualDraftInput, ManualDraftResult, Meta, RuleCatalog, TurnResult } from './types'

// ApiError is a non-2xx answer: its status, the API's error code and message,
// and the whole body (e.g. validation issues).
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly body: any = {},
  ) {
    super(message)
  }
}

async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  const resp = await fetch(url, {
    method,
    headers: body === undefined ? langHeader() : { ...langHeader(), 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    throw new ApiError(resp.status, data.error ?? 'http_' + resp.status, data.message ?? resp.statusText, data)
  }
  return data as T
}

export const api = {
  meta: () => request<Meta>('GET', '/api/meta'),
  createChat: () => request<{ id: string }>('POST', '/api/chats'),
  chats: async (limit = 20) => (await request<{ chats: ChatSummary[] }>('GET', `/api/chats?limit=${limit}`)).chats,
  chat: (id: string) => request<Chat>('GET', `/api/chats/${id}`),
  turn: (id: string) => request<TurnResult>('GET', `/api/turns/${id}`),
  campaign: (id: string) => request<CampaignDetail>('GET', `/api/campaigns/${id}`),
  ruleCatalog: () => request<RuleCatalog>('GET', '/api/campaign-rules/catalog'),
  createCampaign: (draft: ManualDraftInput & { chat_id?: string }) => request<ManualDraftResult>('POST', '/api/campaigns', draft),
  editCampaign: (id: string, edit: Omit<ManualDraftInput, 'segment_rule'> & { base_version_id: number; segment_rule?: ManualDraftInput['segment_rule'] }) =>
    request<ManualDraftResult>('POST', `/api/campaigns/${id}/versions`, edit),
  approve: (id: string, versionId: number, options?: ApproveOptions) =>
    request<ApproveResult>('POST', `/api/campaigns/${id}/approve`, { version_id: versionId, ...options }),
  cancelScheduled: (id: string) => request<{ status: 'cancelled' }>('POST', `/api/campaigns/${id}/cancel`),
  approvalPreview: (id: string, versionId: number) =>
    request<ApprovalPreview>('GET', `/api/campaigns/${id}/approval-preview?version_id=${versionId}`),
  dashboard: () => request<Dashboard>('GET', '/api/dashboard'),
}
