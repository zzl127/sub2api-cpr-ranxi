<template>
  <BaseDialog :show="show" :title="label('CPR 账号接入', 'CPR Account')" width="wide" @close="close">
    <form class="space-y-4" @submit.prevent="save">
      <p class="text-sm text-gray-500">
        {{ label('一个本地账号绑定一个 CPR 账号。请求原样转发，不接管上游身份，也不启用 turn-state 接力。',
          'One local account maps to one CPR account. Payloads are relayed without identity or turn-state rewriting.') }}
      </p>
      <label class="block"><span class="input-label">{{ label('名称', 'Name') }}</span><input v-model="form.name" data-testid="cpr-name" class="input" :disabled="busy" required></label>
      <label class="block"><span class="input-label">{{ label('CPR 网关地址', 'CPR gateway URL') }}</span>
        <input v-model="form.base" data-testid="cpr-base" class="input" :disabled="busy || !!account" placeholder="http://127.0.0.1:18090" required>
      </label>
      <label class="block"><span class="input-label">{{ label('管理地址（可选）', 'Admin URL (optional)') }}</span>
        <input v-model="form.adminBase" class="input" :disabled="busy || !!account">
      </label>
      <label class="block"><span class="input-label">CPR Admin Key</span>
        <input v-model="form.adminKey" data-testid="cpr-admin-key" class="input" type="password" autocomplete="new-password" :disabled="busy" :required="!account"
          :placeholder="account ? label('留空保留原密钥', 'Leave empty to retain') : ''">
      </label>
      <button v-if="!account" class="btn btn-secondary" type="button" :disabled="busy || !form.base || !form.adminKey" @click="discover">
        {{ label('连接并读取账号（只读）', 'Connect and list accounts (read-only)') }}
      </button>
      <label class="block"><span class="input-label">{{ label('CPR 账号', 'CPR account') }}</span>
        <select v-model="form.remoteId" data-testid="cpr-account" class="input" :disabled="busy || !!account || !remotes.length" required>
          <option value="">{{ label('请选择', 'Select an account') }}</option>
          <option v-if="account" :value="form.remoteId">{{ form.remoteId }}</option>
          <option v-for="remote in remotes" :key="remote.id" :value="remote.id">{{ remote.name || remote.email }} · {{ remote.status }}</option>
        </select>
      </label>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <label><span class="input-label">{{ label('并发上限', 'Concurrency') }}</span><input v-model.number="form.concurrency" class="input" type="number" min="1" max="10000" required></label>
        <label><span class="input-label">{{ label('代理（仅 Sub2API 到 CPR）', 'Proxy (Sub2API to CPR only)') }}</span>
          <select v-model="form.proxy" class="input"><option :value="null">{{ label('直连', 'Direct') }}</option><option v-for="p in proxies" :key="p.id" :value="p.id">{{ p.name }}</option></select>
        </label>
      </div>
      <label class="block"><span class="input-label">{{ label('本地 OpenAI 分组（可多选）', 'Local OpenAI groups (multiple)') }}</span>
        <select v-model="form.groups" class="input" multiple><option v-for="g in openAIGroups" :key="g.id" :value="g.id">{{ g.name }}</option></select>
      </label>
      <label class="flex items-center gap-2 text-sm">
        <input v-model="form.longContext" type="checkbox" :disabled="busy">
        {{ label('启用账号长上下文阶梯计费（仍受分组设置控制）', 'Enable account long-context billing (subject to group policy)') }}
      </label>
      <label v-if="!account" class="flex items-start gap-2 text-sm text-amber-700 dark:text-amber-400">
        <input v-model="confirmed" data-testid="cpr-consent" type="checkbox" required class="mt-1">
        {{ label('保存时允许为所选 CPR 账号创建或复用专用分组与客户端密钥；不会删除原账号。',
          'Allow saving to create or reuse a dedicated CPR group and client key. Existing accounts are not deleted.') }}
      </label>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="close">{{ label('取消', 'Cancel') }}</button>
        <button type="submit" class="btn btn-primary" :disabled="busy || !form.remoteId || (!account && !confirmed)">{{ label('保存', 'Save') }}</button>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { create, update } from '@/api/admin/accounts'
import { discoverCPR } from '@/api/admin/cpr'
import type { Account, AdminGroup, Proxy } from '@/types'
import type { CPRRemoteAccount } from '@/types/cpr'

const props = defineProps<{ show: boolean; account?: Account | null; groups: AdminGroup[]; proxies: Proxy[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { locale } = useI18n()
const label = (zh: string, en: string) => locale.value.startsWith('zh') ? zh : en
const openAIGroups = computed(() => props.groups.filter(g => g.platform === 'openai'))
const form = reactive({ name: '', base: '', adminBase: '', adminKey: '', remoteId: '', concurrency: 10, proxy: null as number | null, groups: [] as number[], longContext: false })
const remotes = ref<CPRRemoteAccount[]>([])
const busy = ref(false)
const confirmed = ref(false)
const error = ref('')
let generation = 0

watch(() => props.show, show => {
  generation++
  form.adminKey = ''
  remotes.value = []
  error.value = ''
  busy.value = false
  confirmed.value = false
  if (!show) return
  const a = props.account
  Object.assign(form, { name: a?.name || '', base: String(a?.credentials?.base_url || ''),
    adminBase: String(a?.credentials?.admin_base_url || ''), remoteId: String(a?.credentials?.cpr_account_id || ''),
    concurrency: a?.concurrency || 10, proxy: a?.proxy_id || null, groups: [...(a?.group_ids || a?.groups?.map(g => g.id) || [])],
    longContext: Boolean(a?.extra?.openai_long_context_billing_enabled) })
}, { immediate: true })
watch(() => [form.base, form.adminBase, form.adminKey], () => {
  generation++
  if (!props.account) { form.remoteId = ''; remotes.value = []; confirmed.value = false }
})
function close() { if (!busy.value) { form.adminKey = ''; emit('close') } }
async function discover() {
  const epoch = ++generation
  busy.value = true
  error.value = ''
  try {
    const data = await discoverCPR({ base_url: form.base.trim(), admin_base_url: form.adminBase.trim(), admin_api_key: form.adminKey.trim() })
    if (generation === epoch && props.show) remotes.value = data
  } catch {
    if (generation === epoch) error.value = label('连接失败，请检查地址和管理密钥。', 'Connection failed. Check the URL and Admin Key.')
  } finally { if (generation === epoch) busy.value = false }
}
async function save() {
  if (busy.value || !form.remoteId || (!props.account && !confirmed.value)) return
  busy.value = true
  error.value = ''
  const credentials: Record<string, unknown> = { ...(props.account?.credentials || {}), base_url: form.base.trim(), admin_base_url: form.adminBase.trim(), cpr_account_id: form.remoteId }
  if (form.adminKey.trim()) credentials.admin_api_key = form.adminKey.trim()
  try {
    const common = { name: form.name.trim(), credentials, concurrency: form.concurrency, proxy_id: form.proxy, group_ids: form.groups,
      extra: { ...(props.account?.extra || {}), openai_long_context_billing_enabled: form.longContext } }
    if (props.account) await update(props.account.id, common)
    else await create({ ...common, platform: 'openai', type: 'cpr', priority: 50, cpr_auto_configure: true })
    form.adminKey = ''
    emit('saved')
    emit('close')
  } catch {
    error.value = label('保存失败。请核对配置；重试会复用已创建的专用资源。', 'Save failed. Check configuration; retries reuse dedicated resources.')
  } finally { busy.value = false }
}
</script>
