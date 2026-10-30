<template>
  <BaseDialog :show="show" :title="label('CPR Key 接入', 'CPR Client Key')" width="wide" @close="close">
    <div class="space-y-5">
      <p class="text-sm text-gray-500">
        {{ label('直接使用已有 CPR 客户端 API Key。权限、账号池和选号保持 CPR 原有设置，不读取或修改 CPR 内部账号。',
          'Use an existing CPR client API key. CPR retains its permissions, pool and scheduling; no remote accounts are read or modified.') }}
      </p>
      <section class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-gray-700">
        <div class="flex items-center justify-between gap-2">
          <h3 class="font-medium">{{ label('全局 CPR 网关', 'Global CPR Gateway') }}</h3>
          <button v-if="gatewayLoaded && !gatewayEditor" type="button" class="text-sm text-primary-600" :disabled="busy" data-testid="cpr-edit-gateway" @click="gatewayEditor = true">
            {{ label('修改', 'Edit') }}
          </button>
        </div>
        <p v-if="loadingGateway" class="text-sm">{{ label('读取配置中…', 'Loading settings...') }}</p>
        <p v-if="savedGateway" data-testid="cpr-saved-gateway" class="break-all text-sm">{{ savedGateway }}</p>
        <p class="text-xs text-gray-500">{{ label('地址只需配置一次，所有 CPR Key 共用；保存到本系统，不会修改 CPR。', 'Configure once for all CPR keys. Saved locally; CPR is not modified.') }}</p>
        <template v-if="gatewayLoaded && gatewayEditor">
          <label class="block">
            <span class="input-label">{{ label('网关地址', 'Gateway URL') }}</span>
            <input v-model="gatewayInput" data-testid="cpr-base" class="input" :disabled="busy" placeholder="http://127.0.0.1:18090">
          </label>
          <label v-if="savedGateway && gatewayDirty" class="flex items-start gap-2 text-sm text-amber-700 dark:text-amber-400">
            <input v-model="confirmGatewayChange" data-testid="cpr-gateway-consent" type="checkbox" :disabled="busy" class="mt-1">
            {{ label('确认修改全部 CPR Key 的请求目的地。清空地址将停止新的 CPR 转发。', 'Change the destination for all CPR keys. Clearing the URL stops new CPR forwarding.') }}
          </label>
          <button type="button" class="btn btn-secondary" data-testid="cpr-save-gateway" :disabled="busy || !gatewayDirty || (!!savedGateway && !confirmGatewayChange)" @click="saveGateway">
            {{ label('保存全局地址', 'Save Global URL') }}
          </button>
        </template>
        <div v-if="gatewayError" role="alert" class="text-sm text-red-600">
          {{ gatewayError }}
          <button v-if="!gatewayLoaded" type="button" class="ml-2 underline" :disabled="busy" @click="loadGateway">{{ label('重试', 'Retry') }}</button>
        </div>
      </section>

      <form class="space-y-4" @submit.prevent="save">
        <label class="block"><span class="input-label">{{ label('名称', 'Name') }}</span><input v-model="form.name" data-testid="cpr-name" class="input" :disabled="busy" required></label>
        <label class="block"><span class="input-label">{{ label('CPR 客户端 API Key（不是 Admin Key）', 'CPR Client API Key (not Admin Key)') }}</span>
          <input v-model="form.clientKey" data-testid="cpr-client-key" class="input" type="password" autocomplete="new-password" :disabled="busy" :required="!account || legacyBinding"
            :placeholder="account && !legacyBinding ? label('留空保留已有 Key', 'Leave empty to retain the existing key') : ''">
        </label>
        <p v-if="legacyBinding" class="text-sm text-amber-700 dark:text-amber-400">
          {{ label('这是旧版单账号绑定。请填入你已有的 CPR 账号池 Key；不会自动扩大旧 Key 权限或删除 CPR 资源。',
            'Legacy single-account binding: enter your existing CPR pool key. No key scopes are widened and no CPR resources are deleted.') }}
        </p>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <label><span class="input-label">{{ label('此 Key 的本地并发上限', 'Local Concurrency for This Key') }}</span><input v-model.number="form.concurrency" class="input" type="number" min="1" max="10000" :disabled="busy" required></label>
          <label><span class="input-label">{{ label('代理（仅 Sub2API 到 CPR）', 'Proxy (Sub2API to CPR Only)') }}</span>
            <select v-model="form.proxy" class="input" :disabled="busy"><option :value="null">{{ label('直连', 'Direct') }}</option><option v-for="p in proxies" :key="p.id" :value="p.id">{{ p.name }}</option></select>
          </label>
        </div>
        <label class="block"><span class="input-label">{{ label('本地 OpenAI 分组（可多选）', 'Local OpenAI Groups (Multiple)') }}</span>
          <select v-model="form.groups" class="input" multiple :disabled="busy"><option v-for="g in openAIGroups" :key="g.id" :value="g.id">{{ g.name }}</option></select>
        </label>
        <label class="flex items-center gap-2 text-sm">
          <input v-model="form.longContext" type="checkbox" :disabled="busy">
          {{ label('启用长上下文阶梯计费（仍受分组设置控制）', 'Enable Long-context Billing (Subject to Group Policy)') }}
        </label>
        <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
        <div class="flex justify-end gap-2">
          <button type="button" class="btn btn-secondary" :disabled="savingKey || savingGateway" @click="close">{{ label('取消', 'Cancel') }}</button>
          <button type="submit" class="btn btn-primary" data-testid="cpr-save-key" :disabled="!canSave">{{ label('保存 Key 接入', 'Save Key Channel') }}</button>
        </div>
      </form>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { create, update } from '@/api/admin/accounts'
import { getCPRGateway, updateCPRGateway } from '@/api/admin/cpr'
import type { Account, AdminGroup, Proxy } from '@/types'

const props = defineProps<{ show: boolean; account?: Account | null; groups: AdminGroup[]; proxies: Proxy[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const { locale } = useI18n()
const label = (zh: string, en: string) => locale.value.startsWith('zh') ? zh : en
const openAIGroups = computed(() => props.groups.filter(g => g.platform === 'openai'))
const form = reactive({ name: '', clientKey: '', concurrency: 10, proxy: null as number | null, groups: [] as number[], longContext: false })
const savedGateway = ref('')
const gatewayInput = ref('')
const gatewayLoaded = ref(false)
const gatewayEditor = ref(false)
const confirmGatewayChange = ref(false)
const loadingGateway = ref(false)
const savingGateway = ref(false)
const savingKey = ref(false)
const gatewayError = ref('')
const error = ref('')
const busy = computed(() => loadingGateway.value || savingGateway.value || savingKey.value)
const normalizeURL = (url: string) => url.trim().replace(/\/+$/, '')
const gatewayDirty = computed(() => normalizeURL(gatewayInput.value) !== savedGateway.value)
const legacyBinding = computed(() => Boolean(props.account?.credentials?.cpr_account_id || props.account?.credentials?.base_url || props.account?.credentials?.admin_base_url))
const canSave = computed(() => !busy.value && gatewayLoaded.value && !!savedGateway.value && !gatewayDirty.value &&
  !!form.name.trim() && (!!form.clientKey.trim() || (!!props.account && !legacyBinding.value)))
let generation = 0

function failureMessage(reason: unknown, fallback: string): string {
  const message = (reason as { response?: { data?: { message?: unknown } } })?.response?.data?.message
  return typeof message === 'string' ? message : fallback
}
async function loadGateway() {
  const epoch = generation
  loadingGateway.value = true
  gatewayError.value = ''
  try {
    const data = await getCPRGateway()
    if (epoch !== generation || !props.show) return
    savedGateway.value = normalizeURL(data.base_url)
    gatewayInput.value = savedGateway.value
    gatewayLoaded.value = true
    gatewayEditor.value = !savedGateway.value
  } catch (reason) {
    if (epoch === generation) gatewayError.value = failureMessage(reason, label('读取全局 CPR 配置失败。', 'Failed to load global CPR settings.'))
  } finally { if (epoch === generation) loadingGateway.value = false }
}
watch(() => [props.show, props.account?.id], () => {
  generation++
  Object.assign(form, { name: props.account?.name || '', clientKey: '', concurrency: props.account?.concurrency || 10,
    proxy: props.account?.proxy_id || null, groups: [...(props.account?.group_ids || props.account?.groups?.map(g => g.id) || [])],
    longContext: Boolean(props.account?.extra?.openai_long_context_billing_enabled) })
  savedGateway.value = gatewayInput.value = gatewayError.value = error.value = ''
  gatewayLoaded.value = gatewayEditor.value = confirmGatewayChange.value = false
  savingKey.value = savingGateway.value = loadingGateway.value = false
  if (props.show) void loadGateway()
}, { immediate: true })
watch(gatewayInput, () => { confirmGatewayChange.value = false })

function close() {
  if (savingKey.value || savingGateway.value) return
  generation++
  form.clientKey = ''
  emit('close')
}
async function saveGateway() {
  if (busy.value || !gatewayLoaded.value || !gatewayDirty.value || (savedGateway.value && !confirmGatewayChange.value)) return
  const epoch = generation
  savingGateway.value = true
  gatewayError.value = ''
  try {
    const data = await updateCPRGateway({ base_url: normalizeURL(gatewayInput.value) })
    if (epoch !== generation || !props.show) return
    savedGateway.value = normalizeURL(data.base_url)
    gatewayInput.value = savedGateway.value
    gatewayEditor.value = !savedGateway.value
    confirmGatewayChange.value = false
  } catch (reason) {
    if (epoch === generation) gatewayError.value = failureMessage(reason, label('保存全局地址失败。', 'Failed to save the global URL.'))
  } finally { if (epoch === generation) savingGateway.value = false }
}
async function save() {
  if (!canSave.value) return
  const epoch = generation
  savingKey.value = true
  error.value = ''
  const credentials: Record<string, unknown> = { ...(props.account?.credentials || {}) }
  for (const key of ['base_url', 'admin_base_url', 'admin_api_key', 'cpr_account_id', 'api_key']) delete credentials[key]
  if (form.clientKey.trim()) credentials.api_key = form.clientKey.trim()
  try {
    const common = { name: form.name.trim(), credentials, concurrency: form.concurrency, proxy_id: form.proxy, group_ids: [...form.groups],
      extra: { ...(props.account?.extra || {}), openai_long_context_billing_enabled: form.longContext } }
    if (props.account) await update(props.account.id, common)
    else await create({ ...common, platform: 'openai', type: 'cpr', priority: 50 })
    if (epoch !== generation || !props.show) return
    form.clientKey = ''
    emit('saved')
    emit('close')
  } catch (reason) {
    if (epoch === generation) error.value = failureMessage(reason, label('保存失败，请核对客户端 Key 和全局网关。', 'Save failed. Check the client key and global gateway.'))
  } finally { if (epoch === generation) savingKey.value = false }
}
onBeforeUnmount(() => { generation++; form.clientKey = '' })
</script>
