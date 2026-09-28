import Aura from '@primeuix/themes/aura'
import { definePreset } from '@primeuix/themes'

// Aura (themes v3) resolves colours with CSS light-dark(), driven by the
// color-scheme set on <html> in style.css. The surface scale mirrors the app
// tokens (--iw-*), so PrimeVue widgets and the shell switch themes together.
const light = ['#ffffff', '#f5f7fa', '#edf2f7', '#d9e2ec', '#c3cfdc', '#8a99aa', '#627386', '#4a5a6c', '#344354', '#23303f', '#172331', '#0b1017']
const dark = ['#ffffff', '#ecf2f8', '#d5dee8', '#b4c1cf', '#96a6b8', '#6b7c8f', '#4a5b6e', '#3a4b5f', '#293747', '#1a2532', '#121a24', '#0b1017']
const shades = [0, 50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950]
const surface = Object.fromEntries(shades.map((s, i) => [s, `light-dark(${light[i]}, ${dark[i]})`]))

export const preset = definePreset(Aura, {
  primitive: {
    borderRadius: { none: '0', xs: '2px', sm: '4px', md: '8px', lg: '10px', xl: '14px' },
  },
  semantic: {
    primary: {
      50: '#eef3ff',
      100: '#dce6ff',
      200: '#bccdff',
      300: '#a3bbff',
      400: '#86a5ff',
      500: '#5f84f0',
      600: '#365fd3',
      700: '#2a4fb8',
      800: '#223f92',
      900: '#1b3170',
      950: '#111f47',
      color: 'var(--iw-primary)',
      contrastColor: 'var(--iw-on-primary)',
      hoverColor: 'var(--iw-primary-hover)',
      activeColor: 'var(--iw-primary-hover)',
    },
    surface,
    highlight: {
      background: 'var(--iw-primary-soft)',
      focusBackground: 'var(--iw-primary-soft)',
      color: 'var(--iw-text)',
      focusColor: 'var(--iw-text)',
    },
    text: {
      color: 'var(--iw-text)',
      hoverColor: 'var(--iw-text)',
      mutedColor: 'var(--iw-muted)',
      hoverMutedColor: 'var(--iw-text)',
    },
    content: {
      background: 'var(--iw-surface)',
      hoverBackground: 'var(--iw-hover)',
      borderColor: 'var(--iw-border)',
    },
    formField: {
      background: 'var(--iw-bg)',
      disabledBackground: 'var(--iw-elevated)',
      filledBackground: 'var(--iw-elevated)',
      borderColor: 'var(--iw-border)',
      hoverBorderColor: 'var(--iw-border-strong)',
      focusBorderColor: 'var(--iw-primary)',
      color: 'var(--iw-text)',
      placeholderColor: 'var(--iw-dimmed)',
      shadow: 'none',
    },
    overlay: {
      select: { background: 'var(--iw-surface)', borderColor: 'var(--iw-border)', shadow: 'var(--iw-shadow)' },
      popover: { background: 'var(--iw-surface)', borderColor: 'var(--iw-border)', shadow: 'var(--iw-shadow)' },
      modal: { background: 'var(--iw-surface)', borderColor: 'var(--iw-border)', shadow: 'var(--iw-shadow)' },
    },
    list: {
      option: {
        focusBackground: 'var(--iw-hover)',
      },
    },
  },
})
