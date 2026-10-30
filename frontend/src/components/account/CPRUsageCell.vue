<template>
  <div class="space-y-1 text-xs">
    <div>CPR Key</div>
    <div class="text-gray-500">{{ label('账号池和权限由 CPR 管理', 'Pool and permissions are managed by CPR') }}</div>
    <div class="text-gray-500">{{ label('剩余额度请在 CPR 查看', 'View remaining quota in CPR') }}</div>
  </div>
</template>
<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const { locale } = useI18n()
const label = (zh: string, en: string) => locale.value.startsWith('zh') ? zh : en
</script>
