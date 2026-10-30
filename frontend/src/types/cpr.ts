export interface CPRGatewaySettings {
  base_url: string
}

export interface CPRKeyUsage {
  mode: 'client_key'
  quota_source: 'cpr'
  quota_available: false
}
