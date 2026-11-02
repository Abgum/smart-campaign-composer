<script setup lang="ts">
import { t } from '../i18n'
import { ref } from 'vue'
import { useDialogFocus } from '../lib/dialog'

// ConfirmDialog asks before an action that cannot be taken back, in place of
// the browser's own confirm(): focus starts on "Vazgeç", stays inside, and
// Esc, the backdrop or "Vazgeç" close it without acting. The parent runs the
// action on confirm and closes the dialog when it is done; busy marks the
// action as running.
withDefaults(
  defineProps<{
    title: string
    confirmLabel: string
    busyLabel?: string
    busy?: boolean
    danger?: boolean
  }>(),
  { busyLabel: '', busy: false, danger: false },
)
const emit = defineEmits<{ confirm: []; close: [] }>()

const box = ref<HTMLElement | null>(null)
const { onKey } = useDialogFocus(box, () => emit('close'))
</script>

<template>
  <div class="modal-backdrop" @click.self="emit('close')">
    <div
      ref="box"
      class="modal confirm-dialog"
      role="alertdialog"
      aria-modal="true"
      aria-labelledby="confirm-title"
      aria-describedby="confirm-body"
      tabindex="-1"
      @keydown="onKey"
    >
      <h2 id="confirm-title">{{ title }}</h2>
      <div id="confirm-body"><slot /></div>
      <div class="actions">
        <button type="button" data-autofocus @click="emit('close')">{{ t('Vazgeç') }}</button>
        <button type="button" class="confirm" :class="danger ? 'danger' : 'primary'" :disabled="busy" @click="emit('confirm')">
          {{ busy && busyLabel ? busyLabel : confirmLabel }}
        </button>
      </div>
    </div>
  </div>
</template>
