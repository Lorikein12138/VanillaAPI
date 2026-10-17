// 暖灰：浅色模式文本/边框/底色
const warmGray = {
  50: '#FAF9F5',
  100: '#F0EEE6',
  200: '#E3E1D8',
  300: '#C2C0B6',
  400: '#9C9A92',
  500: '#73726C',
  600: '#5E5D59',
  700: '#3D3D3A',
  800: '#30302E',
  900: '#1F1E1D',
  950: '#141413'
}

// 彩色系：沿用 Tailwind 各档明度，色相收拢到大地色、彩度压低（由 OKLCH 换算生成）
const muted = {
  red: { 50: '#FBF3F2', 100: '#F6E5E2', 200: '#F0D1CB', 300: '#E7B1A9', 400: '#DB887C', 500: '#CE6A5C', 600: '#BB5649', 700: '#9D463C', 800: '#823B32', 900: '#592720', 950: '#381915' },
  rose: { 50: '#FBF2F3', 100: '#F8E7E8', 200: '#F2D3D5', 300: '#E8B0B4', 400: '#DE8890', 500: '#CC6D77', 600: '#B85B66', 700: '#9B4A54', 800: '#7F4148', 900: '#532A2E', 950: '#381E20' },
  pink: { 50: '#FBF3F6', 100: '#F8E9EE', 200: '#F3D4DE', 300: '#EAB3C6', 400: '#D78DA8', 500: '#C47593', 600: '#AF6280', 700: '#95526C', 800: '#794559', 900: '#4E2C39', 950: '#351F27' },
  fuchsia: { 50: '#FCF5FB', 100: '#F9EAF6', 200: '#F2D5ED', 300: '#E3B9DC', 400: '#CD9AC5', 500: '#B580AD', 600: '#9D6996', 700: '#84577D', 800: '#6B4866', 900: '#462F42', 950: '#2F212D' },
  purple: { 50: '#F9F6FC', 100: '#F0EAF8', 200: '#E5D9F3', 300: '#D0BCE8', 400: '#B199CC', 500: '#957BB1', 600: '#80669B', 700: '#6D5685', 800: '#5A496D', 900: '#3D324A', 950: '#2A2332' },
  violet: { 50: '#F5F4FB', 100: '#ECEAF7', 200: '#DCD8F1', 300: '#C2BAE7', 400: '#A398CE', 500: '#8478B0', 600: '#71659C', 700: '#625888', 800: '#514A6F', 900: '#39344C', 950: '#272433' },
  indigo: { 50: '#F0F2FA', 100: '#E4E7F6', 200: '#CDD3EE', 300: '#AEB6E0', 400: '#8C95C6', 500: '#6F78AB', 600: '#5A6294', 700: '#4C537F', 800: '#3F4466', 900: '#32364D', 950: '#202231' },
  blue: { 50: '#F1F6FB', 100: '#E0EAF6', 200: '#C8DAF0', 300: '#A6C4E7', 400: '#80A6D3', 500: '#628AB9', 600: '#4B73A1', 700: '#3F628A', 800: '#36506F', 900: '#28394E', 950: '#1C2734' },
  sky: { 50: '#F3F8FC', 100: '#E6F1F9', 200: '#CBE2F3', 300: '#A6CCE8', 400: '#87B5D7', 500: '#6EA0C4', 600: '#5183A5', 700: '#3D6886', 800: '#37586E', 900: '#263B4A', 950: '#1B2832' },
  cyan: { 50: '#F1FCFF', 100: '#E0F5FA', 200: '#CCEAF1', 300: '#B2DBE5', 400: '#94C7D3', 500: '#77ADBA', 600: '#578C99', 700: '#42717C', 800: '#385C64', 900: '#253C41', 950: '#1B292D' },
  teal: { 50: '#F3FCFB', 100: '#E0F4F2', 200: '#CCE8E6', 300: '#B1D9D5', 400: '#94C4C0', 500: '#77ABA7', 600: '#588B87', 700: '#43706D', 800: '#375956', 900: '#273D3B', 950: '#1C2928' },
  emerald: { 50: '#F1FBF5', 100: '#E1F4E9', 200: '#CCE7D8', 300: '#B2D7C1', 400: '#94C1A8', 500: '#7AAA8F', 600: '#5C8B72', 700: '#47705A', 800: '#395847', 900: '#293D32', 950: '#1B2821' },
  green: { 50: '#F5FBF4', 100: '#EAF7E6', 200: '#D9EDD3', 300: '#C3DEBA', 400: '#A8C99E', 500: '#8FB183', 600: '#729367', 700: '#58744E', 800: '#465C3E', 900: '#2E3D2A', 950: '#1F291C' },
  lime: { 50: '#F9FCEF', 100: '#F1F8E1', 200: '#E6F0CF', 300: '#D8E4B7', 400: '#C7D59F', 500: '#ADBC82', 600: '#88965E', 700: '#677342', 800: '#525B36', 900: '#353B23', 950: '#252819' },
  yellow: { 50: '#FFFBF0', 100: '#FFF5E0', 200: '#FCECC4', 300: '#F5DEA7', 400: '#EACE8A', 500: '#D7B96E', 600: '#AF9556', 700: '#85703D', 800: '#6C5A30', 900: '#433617', 950: '#2D2512' },
  amber: { 50: '#FFFAF4', 100: '#FFF0DF', 200: '#FFE0BC', 300: '#FACF9B', 400: '#F3BF7E', 500: '#DCA968', 600: '#B98A4C', 700: '#946A32', 800: '#765426', 900: '#493315', 950: '#322311' },
  orange: { 50: '#FEF7F3', 100: '#FEECE2', 200: '#F8D7C4', 300: '#F1BCA0', 400: '#E39E77', 500: '#D9895B', 600: '#C87646', 700: '#A45E34', 800: '#824B2A', 900: '#522D17', 950: '#371F11' }
}

/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        ...muted,
        // 主色调 - Clay 陶土色（Anthropic 编辑风强调色）
        primary: {
          50: '#FBF5F1',
          100: '#F7E7DF',
          200: '#F0CDBD',
          300: '#E7AC93',
          400: '#D97757',
          500: '#C6613F',
          600: '#B5532F',
          700: '#9A4527',
          800: '#7C3920',
          900: '#5F2D1B',
          950: '#351810'
        },
        // 中性色 - 暖灰（覆盖 Tailwind 默认冷灰）
        gray: warmGray,
        slate: warmGray,
        zinc: warmGray,
        neutral: warmGray,
        accent: warmGray,
        // 深色模式背景（暖炭色；900 为侧栏，950 为页面底色）
        dark: {
          50: '#FAF9F5',
          100: '#F0EEE6',
          200: '#DEDCD1',
          300: '#C2C0B6',
          400: '#9C9A92',
          500: '#6B6A65',
          600: '#4D4C48',
          700: '#3D3D3A',
          800: '#30302E',
          900: '#1F1E1D',
          950: '#262624'
        }
      },
      fontFamily: {
        sans: [
          'system-ui',
          '-apple-system',
          'BlinkMacSystemFont',
          'Segoe UI',
          'Roboto',
          'Helvetica Neue',
          'Arial',
          'PingFang SC',
          'Hiragino Sans GB',
          'Microsoft YaHei',
          'sans-serif'
        ],
        serif: [
          'Georgia',
          'Noto Serif SC',
          'Source Han Serif SC',
          'Songti SC',
          'PingFang SC',
          'Microsoft YaHei',
          'serif'
        ],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Monaco', 'Consolas', 'monospace']
      },
      boxShadow: {
        glass: '0 1px 2px rgba(20, 20, 19, 0.04), 0 8px 24px rgba(20, 20, 19, 0.06)',
        'glass-sm': '0 1px 2px rgba(20, 20, 19, 0.04), 0 4px 12px rgba(20, 20, 19, 0.05)',
        glow: '0 0 0 1px rgba(198, 97, 63, 0.25)',
        'glow-lg': '0 0 0 1px rgba(198, 97, 63, 0.35)',
        card: '0 1px 2px rgba(20, 20, 19, 0.03)',
        'card-hover': '0 4px 16px rgba(20, 20, 19, 0.06)',
        'inner-glow': 'none'
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(var(--tw-gradient-stops))',
        'gradient-primary': 'linear-gradient(135deg, #C6613F 0%, #B5532F 100%)',
        'gradient-dark': 'linear-gradient(135deg, #30302E 0%, #262624 100%)',
        'gradient-glass': 'none',
        'mesh-gradient': 'none'
      },
      animation: {
        'fade-in': 'fadeIn 0.3s ease-out',
        'slide-up': 'slideUp 0.3s ease-out',
        'slide-down': 'slideDown 0.3s ease-out',
        'slide-in-right': 'slideInRight 0.3s ease-out',
        'scale-in': 'scaleIn 0.2s ease-out',
        'pulse-slow': 'pulse 3s cubic-bezier(0.4, 0, 0.6, 1) infinite',
        shimmer: 'shimmer 2s linear infinite',
        glow: 'glow 2s ease-in-out infinite alternate'
      },
      keyframes: {
        fadeIn: {
          '0%': { opacity: '0' },
          '100%': { opacity: '1' }
        },
        slideUp: {
          '0%': { opacity: '0', transform: 'translateY(10px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' }
        },
        slideDown: {
          '0%': { opacity: '0', transform: 'translateY(-10px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' }
        },
        slideInRight: {
          '0%': { opacity: '0', transform: 'translateX(20px)' },
          '100%': { opacity: '1', transform: 'translateX(0)' }
        },
        scaleIn: {
          '0%': { opacity: '0', transform: 'scale(0.95)' },
          '100%': { opacity: '1', transform: 'scale(1)' }
        },
        shimmer: {
          '0%': { backgroundPosition: '-200% 0' },
          '100%': { backgroundPosition: '200% 0' }
        },
        glow: {
          '0%': { boxShadow: '0 0 0 1px rgba(198, 97, 63, 0.2)' },
          '100%': { boxShadow: '0 0 0 3px rgba(198, 97, 63, 0.3)' }
        }
      },
      backdropBlur: {
        xs: '2px'
      },
      // 编辑风：收紧圆角
      borderRadius: {
        xl: '0.5rem',
        '2xl': '0.625rem',
        '3xl': '0.75rem',
        '4xl': '1rem'
      }
    }
  },
  plugins: []
}
