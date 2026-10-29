import type { UsageProgress } from './index'

export interface CPRRemoteAccount {
  id: string
  name: string
  email: string
  plan_type: string
  status: string
  enabled: boolean
  concurrency_limit?: number | null
}

export interface CPRQuotaSnapshot {
  account_id: string
  name?: string
  email?: string
  plan_type?: string
  status?: string
  enabled: boolean
  outbound_proxy?: string
  quota_updated_display?: string
  checked_at?: string
  query_attempted_at?: string
  query_error?: string
  five_hour?: UsageProgress
  seven_day?: UsageProgress
}
