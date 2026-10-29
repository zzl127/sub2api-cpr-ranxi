<template>
  <div class="space-y-1 text-xs">
    <div>{{ snapshot?.plan_type || 'CPR' }} · {{ snapshot?.status || label('未知', 'Unknown') }}</div>
    <div>5h: {{ percent(snapshot?.five_hour?.utilization) }} · 7d: {{ percent(snapshot?.seven_day?.utilization) }}</div>
    <div v-if="snapshot?.quota_updated_display" class="text-gray-500">{{ label('CPR 返回时的额度年龄', 'Quota age when CPR replied') }}: {{ snapshot.quota_updated_display }}</div>
    <div v-if="snapshot?.checked_at" class="text-gray-500">{{ label('上次查询成功', 'Last successful query') }}: {{ new Date(snapshot.checked_at).toLocaleString() }}</div>
    <div v-if="snapshot?.outbound_proxy" class="max-w-56 truncate text-gray-500" :title="snapshot.outbound_proxy">{{ snapshot.outbound_proxy }}</div>
    <div v-if="snapshot?.query_error || error" class="text-amber-600">{{ snapshot?.query_error || error }}</div>
    <button class="text-primary-600 hover:underline" :disabled="loading" @click="load(true)">{{ label('查询 CPR', 'Query CPR') }}</button>
  </div>
</template>
<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getUsage } from '@/api/admin/accounts'
import type { CPRQuotaSnapshot } from '@/types/cpr'
const props = defineProps<{ accountId: number }>()
const { locale } = useI18n()
const label = (zh: string, en: string) => locale.value.startsWith('zh') ? zh : en
const snapshot = ref<CPRQuotaSnapshot>()
const loading = ref(false)
const error = ref('')
let generation = 0
const timer = setInterval(() => {
  if (!loading.value && document.visibilityState === 'visible') void load()
}, 60000)
const percent = (value?: number) => typeof value === 'number' ? `${value.toFixed(1)}%` : '—'
async function load(force = false) {
  const epoch = ++generation
  loading.value = true
  error.value = ''
  try {
    const result = await getUsage(props.accountId, 'active', force)
    if (epoch === generation) snapshot.value = result.cpr
  } catch {
    if (epoch === generation) error.value = label('查询失败，额度未知', 'Query failed; quota unknown')
  } finally { if (epoch === generation) loading.value = false }
}
watch(() => props.accountId, () => { snapshot.value = undefined; void load() }, { immediate: true })
onBeforeUnmount(() => { generation++; clearInterval(timer) })
</script>
