<script setup lang="ts">
import { t, tk } from '../i18n'
import { computed, nextTick, ref, watch } from 'vue'
import { useChatStore } from '../stores/chat'
import Icon from './Icon.vue'
import MessageBubble from './MessageBubble.vue'
import TurnPreview from './TurnPreview.vue'
import TargetIndicator from './TargetIndicator.vue'
import AmbiguousChips from './AmbiguousChips.vue'
import RouteLine from './RouteLine.vue'

const store = useChatStore()
const draft = ref('')
const scenario = ref('')
const list = ref<HTMLElement | null>(null)
const input = ref<HTMLTextAreaElement | null>(null)

const scenarios = computed(() => store.meta?.mock_scenarios ?? [])
const lastIndex = computed(() => store.messages.length - 1)

// A new chat offers a few briefs to start from; picking one only fills the
// message box, so it can be changed before it is sent.
const examples = [
  tk("İstanbul'da son 30 günde sepete ürün ekleyip satın almayanlara SMS ve e-posta hazırla."),
  tk("Ankara'da son bir ayda üye olanlara hoş geldin kampanyası hazırla."),
  tk("İzmir'de en az 3 sipariş vermiş müşterilere teşekkür mesajı yaz."),
]
function useExample(text: string) {
  draft.value = text
  input.value?.focus()
}

// Only the latest failed turn can be retried, and not while a turn runs.
function retryable(i: number) {
  const m = store.messages[i]
  return i === lastIndex.value && m.role === 'assistant' && m.outcome === 'failed' && !store.turn
}

async function submit() {
  let text = draft.value.trim()
  if (!text || store.turn || store.unsavedEdit) return
  if (scenario.value) text += ` #scenario:${scenario.value}`
  draft.value = ''
  await store.send(text)
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    void submit()
  }
}

watch(
  () => [store.messages.length, store.turn?.preview.text.length, store.turn?.preview.drafts.length, store.question],
  async () => {
    await nextTick()
    list.value?.scrollTo({ top: list.value.scrollHeight })
  },
)
</script>

<template>
  <section class="chat-panel" :aria-label="t('Sohbet')">
    <header class="panel-head">
      <span class="panel-icon" aria-hidden="true"><Icon name="assistant" :size="18" /></span>
      <h2 class="panel-title">{{ t('Sohbet') }}</h2>
      <span v-if="!store.isMock" class="panel-hint">{{ t('Ne istediğinizi yazın; taslak yanda açılır.') }}</span>
    </header>
    <p v-if="store.isMock" class="panel-hint mock">
      <Icon name="info" :size="14" />{{ t('Demo modeli mesajlarınızı yorumlamaz; hazır taslakları sırayla gösterir.') }}
    </p>

    <div ref="list" class="messages" role="log" :aria-label="t('Sohbet mesajları')" aria-relevant="additions" tabindex="0">
      <div v-if="!store.messages.length && !store.turn" class="chat-empty">
        <span class="empty-icon" aria-hidden="true"><Icon name="assistant" :size="22" /></span>
        <h3>{{ t('Kampanyanızı anlatın') }}</h3>
        <p>{{ t('Kime, hangi kanaldan, ne söylemek istediğinizi yazın. Asistan hedef kitleyi ve SMS/e-posta taslağını hazırlar; siz inceleyip onaylarsınız.') }}</p>
        <div class="examples">
          <button v-for="e in examples" :key="e" type="button" class="example-chip" @click="useExample(t(e))">{{ t(e) }}</button>
        </div>
      </div>
      <template v-for="(m, i) in store.messages" :key="m.turn_id + m.role">
        <MessageBubble :message="m" :retryable="retryable(i)" @retry="store.retry(m.turn_id)" />
        <RouteLine v-if="i === lastIndex && m.role === 'assistant' && store.lastRoute && !store.turn" :route="store.lastRoute" />
      </template>
      <AmbiguousChips v-if="store.question" :candidates="store.question.candidates ?? []" @choose="store.choose" />
      <TurnPreview v-if="store.turn" :turn="store.turn" />
    </div>

    <div v-if="store.notice" class="notice" role="alert">
      {{ t(store.notice.text) }}
      <button v-if="store.notice.reconnect" type="button" class="link" @click="store.reconnect()">{{ t('Yeniden bağlan') }}</button>
    </div>

    <form class="composer" @submit.prevent="submit">
      <TargetIndicator :focus="store.focus" @clear="store.selectCampaign(null)" />
      <textarea
        ref="input"
        v-model="draft"
        rows="3"
        :disabled="!!store.turn"
        :placeholder="t('Örn. İstanbul\'da sepeti terk edenlere SMS hazırla… (Enter gönderir, Shift+Enter yeni satır)')"
        :aria-label="t('Mesaj')"
        @keydown="onKey"
      />
      <p v-if="store.unsavedEdit" class="muted small" role="status">
        {{ t('Taslakta kaydedilmemiş değişiklik var; mesaj göndermeden önce kaydedin ya da geri alın.') }}
      </p>
      <div class="composer-row">
        <label v-if="scenarios.length" class="scenario" :title="t('Yalnızca demo modunda: gösterilecek örnek senaryo')">
          {{ t('Demo senaryosu') }}
          <select v-model="scenario">
            <option value="">{{ t('Geçerli (varsayılan)') }}</option>
            <option v-for="s in scenarios.filter((x) => x.name !== 'default')" :key="s.name" :value="s.name" :title="s.description">
              {{ s.name }}
            </option>
          </select>
        </label>
        <span v-if="scenario" class="muted scenario-help">{{ scenarios.find((s) => s.name === scenario)?.description }}</span>
        <button type="submit" class="primary send" :disabled="!!store.turn || store.unsavedEdit || !draft.trim()">
          <Icon name="send" :size="16" />{{ store.turn ? t('Yanıt bekleniyor…') : t('Gönder') }}
        </button>
      </div>
    </form>
  </section>
</template>
