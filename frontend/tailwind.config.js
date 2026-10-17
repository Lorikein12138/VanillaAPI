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

/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
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
