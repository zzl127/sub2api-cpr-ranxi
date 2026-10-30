import { apiClient } from '../client'
import type { CPRGatewaySettings } from '@/types/cpr'

export async function getCPRGateway(): Promise<CPRGatewaySettings> {
  const { data } = await apiClient.get<CPRGatewaySettings>('/admin/accounts/cpr/gateway')
  return data
}

export async function updateCPRGateway(input: CPRGatewaySettings): Promise<CPRGatewaySettings> {
  const { data } = await apiClient.put<CPRGatewaySettings>('/admin/accounts/cpr/gateway', input)
  return data
}
