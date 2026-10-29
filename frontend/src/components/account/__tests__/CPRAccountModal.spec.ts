import { mount, flushPromises } from '@vue/test-utils'
import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CPRAccountModal from '../CPRAccountModal.vue'
import { create, update } from '@/api/admin/accounts'
import { discoverCPR } from '@/api/admin/cpr'
import type { Account } from '@/types'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: ref('zh-CN') }) }))
vi.mock('@/api/admin/accounts', () => ({ create: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin/cpr', () => ({ discoverCPR: vi.fn() }))

function render(account?: Account) {
  return mount(CPRAccountModal, {
    props: { show: true, account, groups: [], proxies: [] },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } }
  })
}

async function prepare() {
  const wrapper = render()
  await wrapper.get('[data-testid="cpr-name"]').setValue('Fixture')
  await wrapper.get('[data-testid="cpr-base"]').setValue('http://127.0.0.1:18090')
  await wrapper.get('[data-testid="cpr-admin-key"]').setValue('test-admin')
  await wrapper.findAll('button').find(b => b.text().includes('只读'))!.trigger('click')
  await flushPromises()
  await wrapper.get('[data-testid="cpr-account"]').setValue('acct_fixture')
  return wrapper
}

describe('CPR account consent and credential handling', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(discoverCPR).mockResolvedValue([{ id: 'acct_fixture', name: 'Remote fixture', email: 'test@example.invalid', enabled: true, status: 'normal', plan_type: 'pro' }])
    vi.mocked(create).mockResolvedValue({ id: 7 } as Account)
    vi.mocked(update).mockResolvedValue({ id: 7 } as Account)
  })

  it('discovers without creating remote or local accounts', async () => {
    const wrapper = await prepare()
    expect(discoverCPR).toHaveBeenCalledWith({ base_url: 'http://127.0.0.1:18090', admin_base_url: '', admin_api_key: 'test-admin' })
    expect(create).not.toHaveBeenCalled()
    await wrapper.get('form').trigger('submit')
    expect(create).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('requires confirmation and delegates client-key provisioning to the backend', async () => {
    const wrapper = await prepare()
    await wrapper.get('[data-testid="cpr-consent"]').setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      type: 'cpr', platform: 'openai', cpr_auto_configure: true,
      credentials: { base_url: 'http://127.0.0.1:18090', admin_base_url: '', cpr_account_id: 'acct_fixture', admin_api_key: 'test-admin' }
    }))
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect((wrapper.get('[data-testid="cpr-admin-key"]').element as HTMLInputElement).value).toBe('')
    wrapper.unmount()
  })

  it('preserves hidden configuration and never sends a blank replacement secret on edit', async () => {
    const wrapper = render({
      id: 7, type: 'cpr', platform: 'openai', name: 'Existing', concurrency: 10,
      group_ids: [3], proxy_id: null,
      credentials: { base_url: 'http://127.0.0.1:18090', cpr_account_id: 'acct_fixture', model_mapping: { alias: 'gpt-6-astra' } }
    } as unknown as Account)
    await wrapper.get('[data-testid="cpr-name"]').setValue('Renamed')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(7, expect.objectContaining({
      name: 'Renamed',
      credentials: expect.objectContaining({ model_mapping: { alias: 'gpt-6-astra' } }),
      group_ids: [3]
    }))
    expect(vi.mocked(update).mock.calls[0][1].credentials).not.toHaveProperty('admin_api_key')
    expect(create).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
