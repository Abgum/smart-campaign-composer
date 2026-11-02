<script setup lang="ts">
import { t } from '../../i18n'
import { computed } from 'vue'
import type { Audience, CampaignStatus } from '../../api/types'
import { formatCount } from '../../lib/format'
import { dateTimeText, scheduledLater, statusLabel } from '../../lib/schedule'
import Icon from '../Icon.vue'

// A draft's one decision, always in reach at the foot of the canvas:
// approve and send. Unsaved edits are saved first, so what is approved is
// exactly what is shown. Recipients waiting for a language decision lock the
// approval: the bar says so, and the dialog it opens asks for the decision.
// Once approved, the bar says where the campaign is.
const props = withDefaults(
  defineProps<{
    status: CampaignStatus
    dirty: boolean
    busy: boolean
    audience: Audience | null
    waiting?: number
    scheduledAt?: string | null // a scheduled campaign: when sending starts
  }>(),
  { waiting: 0, scheduledAt: null },
)
defineEmits<{ approve: []; cancel: [] }>()

const reach = computed(() => {
  const a = props.audience
  if (!a) return t('Kitle onay anında yeniden hesaplanır.')
  return t("{total} kişi · SMS'e uygun {sms} · e-postaya uygun {email}", { total: formatCount(a.total), sms: formatCount(a.sms_reachable), email: formatCount(a.email_reachable) })
})
</script>

<template>
  <div class="approve-bar" :class="status">
    <template v-if="status === 'draft'">
      <div class="approve-text">
        <strong>{{ dirty ? t('Kaydedilmemiş değişiklik var') : waiting ? t('Dil kararı bekleniyor') : t('Onaya hazır') }}</strong>
        <span>{{
          dirty
            ? t('Önce değişiklikleri kaydedin; onaylanan, burada görünen metnin aynısıdır.')
            : waiting
              ? t('{n} alıcı dil kararı bekliyor; kararı onay penceresinde verebilirsiniz.', { n: formatCount(waiting) })
              : reach
        }}</span>
      </div>
      <button type="button" class="primary approve" :disabled="dirty || busy" @click="$emit('approve')">
        <Icon name="check" :size="18" />{{ waiting ? t('Dil kararı ver') : t('Onayla ve gönder') }}
      </button>
    </template>
    <div v-else class="approve-text">
      <span class="status-chip" :class="status">
        <span v-if="status === 'sending'" class="live-dot" aria-hidden="true" />{{ statusLabel(status, scheduledAt) }}
      </span>
      <span v-if="status === 'approved' && scheduledAt && scheduledLater(scheduledAt)">
        {{ t('Gönderim {when} tarihinde başlayacak; metin artık değiştirilemez.', { when: dateTimeText(scheduledAt) }) }}
      </span>
      <span v-else>{{ status === 'cancelled' ? t('Kampanya iptal edildi.') : t('Onaylandı; metin artık değiştirilemez.') }}</span>
    </div>
    <button
      v-if="status === 'approved' && scheduledAt && scheduledLater(scheduledAt)"
      type="button"
      class="button"
      data-test="cancel-scheduled"
      :disabled="busy"
      @click="$emit('cancel')"
    >{{ t('Gönderimi iptal et') }}</button>
  </div>
</template>
