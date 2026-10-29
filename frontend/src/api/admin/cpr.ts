import { apiClient } from '../client'
import type { CPRRemoteAccount } from '@/types/cpr'

export async function discoverCPR(input: {
  base_url: string
  admin_base_url: string
  admin_api_key: string
}): Promise<CPRRemoteAccount[]> {
  const { data } = await apiClient.post<CPRRemoteAccount[]>('/admin/accounts/cpr/discover', input)
  return data
}
