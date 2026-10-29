import { describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'

describe('patched Vue I18n runtime', () => {
  it('keeps composition-mode interpolation and locale switching', () => {
    const i18n = createI18n({
      legacy: false,
      locale: 'en',
      messages: {
        en: { greeting: 'Hello {name}' },
        zh: { greeting: 'Welcome {name}' }
      }
    })
    expect(i18n.global.t('greeting', { name: 'Ada' })).toBe('Hello Ada')
    i18n.global.locale.value = 'zh'
    expect(i18n.global.t('greeting', { name: 'Ada' })).toBe('Welcome Ada')
    i18n.dispose()
  })

  it.each(['srcdoc', 'SRCDOC'])('neutralizes an iframe %s attribute after interpolation', (attribute) => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const i18n = createI18n({
      legacy: false,
      locale: 'en',
      warnHtmlMessage: false,
      escapeParameter: true,
      messages: { en: { embedded: `<iframe ${attribute}="{content}"></iframe>` } }
    })
    try {
      const translated = i18n.global.t('embedded', {
        content: '<img src=x onerror=alert(1)>'
      })
      const template = document.createElement('template')
      template.innerHTML = translated
      const frame = template.content.querySelector('iframe')
      expect(frame).not.toBeNull()
      expect(frame!.hasAttribute('srcdoc')).toBe(false)
    } finally {
      i18n.dispose()
      warning.mockRestore()
    }
  })
})
