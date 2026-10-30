import { mount, flushPromises } from '@vue/test-utils'
import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CPRAccountModal from '../CPRAccountModal.vue'
import CPRUsageCell from '../CPRUsageCell.vue'
import { create, update } from '@/api/admin/accounts'
import { getCPRGateway, updateCPRGateway } from '@/api/admin/cpr'
import type { Account } from '@/types'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: ref('zh-CN') }) }))
vi.mock('@/api/admin/accounts', () => ({ create: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin/cpr', () => ({ getCPRGateway: vi.fn(), updateCPRGateway: vi.fn() }))

function render(account?: Account) {
  return mount(CPRAccountModal, {
    props: { show: true, account, groups: [], proxies: [] },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } }
  })
}

async function prepare() {
  const wrapper = render()
  await flushPromises()
  await wrapper.get('[data-testid="cpr-name"]').setValue('Pool key')
  await wrapper.get('[data-testid="cpr-client-key"]').setValue('existing-pool-client-key')
  return wrapper
}

describe('CPR client-key integration', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.mocked(getCPRGateway).mockResolvedValue({ base_url: 'http://127.0.0.1:18090' })
    vi.mocked(updateCPRGateway).mockImplementation(async input => input)
    vi.mocked(create).mockResolvedValue({ id: 7 } as Account)
    vi.mocked(update).mockResolvedValue({ id: 7 } as Account)
  })

  it('uses the saved global gateway and needs no remote account or admin credential', async () => {
    const wrapper = await prepare()
    expect(wrapper.find('[data-testid="cpr-admin-key"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="cpr-account"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="cpr-base"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="cpr-saved-gateway"]').text()).toBe('http://127.0.0.1:18090')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      type: 'cpr', platform: 'openai', credentials: { api_key: 'existing-pool-client-key' }
    }))
    expect(vi.mocked(create).mock.calls[0][0]).not.toHaveProperty('cpr_auto_configure')
    expect(updateCPRGateway).not.toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect((wrapper.get('[data-testid="cpr-client-key"]').element as HTMLInputElement).value).toBe('')
    wrapper.unmount()
  })

  it('saves the global URL separately before accepting the first client key', async () => {
    vi.mocked(getCPRGateway).mockResolvedValue({ base_url: '' })
    const wrapper = await prepare()
    await wrapper.get('form').trigger('submit')
    expect(create).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="cpr-base"]').setValue('http://127.0.0.1:18090/')
    await wrapper.get('[data-testid="cpr-save-gateway"]').trigger('click')
    await flushPromises()
    expect(updateCPRGateway).toHaveBeenCalledWith({ base_url: 'http://127.0.0.1:18090' })
    expect(create).not.toHaveBeenCalled()
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(create).toHaveBeenCalledTimes(1)
    expect(vi.mocked(create).mock.calls[0][0].credentials).not.toHaveProperty('base_url')
    wrapper.unmount()
  })

  it('requires explicit confirmation before changing the shared destination', async () => {
    const wrapper = await prepare()
    await wrapper.get('[data-testid="cpr-edit-gateway"]').trigger('click')
    await wrapper.get('[data-testid="cpr-base"]').setValue('https://second.example.invalid')
    await wrapper.get('[data-testid="cpr-save-gateway"]').trigger('click')
    await wrapper.get('form').trigger('submit')
    expect(updateCPRGateway).not.toHaveBeenCalled()
    expect(create).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="cpr-gateway-consent"]').setValue(true)
    await wrapper.get('[data-testid="cpr-save-gateway"]').trigger('click')
    await flushPromises()
    expect(updateCPRGateway).toHaveBeenCalledWith({ base_url: 'https://second.example.invalid' })
    wrapper.unmount()
  })

  it('preserves hidden configuration without sending an empty replacement key', async () => {
    const wrapper = render({
      id: 7, type: 'cpr', platform: 'openai', name: 'Existing', concurrency: 10,
      group_ids: [3], proxy_id: null,
      credentials: { model_mapping: { alias: 'gpt-6-astra' } }
    } as unknown as Account)
    await flushPromises()
    await wrapper.get('[data-testid="cpr-name"]').setValue('Renamed')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(7, expect.objectContaining({
      name: 'Renamed', credentials: { model_mapping: { alias: 'gpt-6-astra' } }, group_ids: [3]
    }))
    expect(create).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('requires an existing pool key to replace a legacy single-account binding', async () => {
    const wrapper = render({
      id: 7, type: 'cpr', platform: 'openai', name: 'Legacy', concurrency: 10,
      credentials: { base_url: 'https://legacy.invalid', admin_base_url: 'https://admin.invalid', cpr_account_id: 'acct_old' }
    } as unknown as Account)
    await flushPromises()
    await wrapper.get('form').trigger('submit')
    expect(update).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="cpr-client-key"]').setValue('existing-new-pool-key')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(update).toHaveBeenCalledWith(7, expect.objectContaining({ credentials: { api_key: 'existing-new-pool-key' } }))
    wrapper.unmount()
  })

  it('blocks key creation if global settings cannot be read and exposes save errors', async () => {
    vi.mocked(getCPRGateway).mockRejectedValueOnce(new Error('offline'))
    const wrapper = await prepare()
    await wrapper.get('form').trigger('submit')
    expect(create).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('读取全局 CPR 配置失败')
    await wrapper.findAll('button').find(button => button.text() === '重试')!.trigger('click')
    await flushPromises()
    vi.mocked(create).mockRejectedValueOnce({ response: { data: { message: 'CPR client API key is required' } } })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('CPR client API key is required')
    expect(wrapper.emitted('saved')).toBeUndefined()
    wrapper.unmount()
  })

  it('ignores a late settings read after closing the dialog', async () => {
    let resolve!: (value: { base_url: string }) => void
    vi.mocked(getCPRGateway).mockImplementationOnce(() => new Promise(done => { resolve = done }))
    const wrapper = render()
    await wrapper.setProps({ show: false })
    resolve({ base_url: 'https://late.example.invalid' })
    await flushPromises()
    expect(wrapper.find('[data-testid="cpr-saved-gateway"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not display single-account quota or poll CPR', () => {
    const wrapper = mount(CPRUsageCell)
    expect(wrapper.text()).toContain('剩余额度请在 CPR 查看')
    expect(wrapper.text()).not.toContain('5h')
    expect(wrapper.find('button').exists()).toBe(false)
    expect(getCPRGateway).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
