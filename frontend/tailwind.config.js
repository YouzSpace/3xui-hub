/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['src/**/*.{vue,js}'],
  theme: {
    extend: {
      colors: {
        base: 'var(--bg-base)',
        surface: 'var(--bg-surface)',
        elevated: 'var(--bg-elevated)',
        input: 'var(--bg-input)',
        hover: 'var(--bg-hover)',
        accent: {
          DEFAULT: 'var(--accent)',
          hover: 'var(--accent-hover)',
          active: 'var(--accent-active)',
          muted: 'var(--accent-muted)',
        },
        border: {
          subtle: 'var(--border-subtle)',
          strong: 'var(--border-strong)',
          focus: 'var(--border-focus)',
        },
        text: {
          primary: 'var(--text-primary)',
          secondary: 'var(--text-secondary)',
          muted: 'var(--text-muted)',
          inverse: 'var(--text-inverse)',
        },
        success: 'var(--success)',
        'success-muted': 'var(--success-muted)',
        warning: 'var(--warning)',
        'warning-muted': 'var(--warning-muted)',
        danger: 'var(--danger)',
        'danger-muted': 'var(--danger-muted)',
        info: 'var(--info)',
        'info-muted': 'var(--info-muted)',
        vless: 'var(--proto-vless)',
        'vless-muted': 'var(--proto-vless-muted)',
        trojan: 'var(--proto-trojan)',
        'trojan-muted': 'var(--proto-trojan-muted)',
      },
      fontFamily: {
        sans: 'var(--font-sans)',
        mono: 'var(--font-mono)',
      },
      borderRadius: {
        sm: 'var(--radius-sm)',
        md: 'var(--radius-md)',
        lg: 'var(--radius-lg)',
        xl: 'var(--radius-xl)',
        full: 'var(--radius-full)',
      },
      boxShadow: {
        sm: 'var(--shadow-sm)',
        md: 'var(--shadow-md)',
        lg: 'var(--shadow-lg)',
        'glow-accent': 'var(--glow-accent)',
        'glow-success': 'var(--glow-success)',
      },
    },
  },
  plugins: [],
}
