<template>
  <div class="card p-4">
    <h3 class="mb-4 text-[0.9375rem] font-serif font-normal text-gray-950 dark:text-dark-50">
      {{ t('payment.admin.dailyRevenue') }}
    </h3>
    <div class="h-64">
      <div v-if="loading" class="flex h-full items-center justify-center">
        <LoadingSpinner size="md" />
      </div>
      <Line v-else-if="chartData" :data="chartData" :options="chartOptions" />
      <div
        v-else
        class="flex h-full items-center justify-center text-sm text-gray-500 dark:text-gray-400"
      >
        {{ t('payment.admin.noData') }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  PointElement,
  LineElement,
  Tooltip,
  Legend,
  Filler
} from 'chart.js'
import { Line } from 'vue-chartjs'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import type { DailyPaymentStats } from '@/types/payment'

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, Tooltip, Legend, Filler)

const { t } = useI18n()

const props = defineProps<{
  data: DailyPaymentStats[]
  loading?: boolean
}>()

const colors = [
  ['rgb(217, 119, 87)', 'rgba(217, 119, 87, 0.1)'],
  ['rgb(106, 155, 204)', 'rgba(106, 155, 204, 0.1)'],
  ['rgb(212, 162, 127)', 'rgba(212, 162, 127, 0.1)'],
  ['rgb(196, 102, 134)', 'rgba(196, 102, 134, 0.1)'],
]

const chartData = computed(() => {
  if (!props.data || props.data.length === 0) return null
  const currencies = [...new Set(props.data.flatMap(day => Object.keys(day.amount)))].sort()
  return {
    labels: props.data.map(d => d.date),
    datasets: [
      ...currencies.map((currency, index) => {
        const [borderColor, backgroundColor] = colors[index % colors.length]
        return {
          label: `${currency} ${t('payment.admin.revenue')}`,
          data: props.data.map(day => day.amount[currency] || 0),
          borderColor,
          backgroundColor,
          fill: true,
          tension: 0.3,
          pointRadius: 3,
          pointHoverRadius: 5,
        }
      }),
      {
        label: t('payment.admin.orderCount'),
        data: props.data.map(d => d.count),
        borderColor: 'rgb(120, 140, 93)',
        backgroundColor: 'rgba(120, 140, 93, 0.1)',
        fill: false,
        tension: 0.3,
        pointRadius: 3,
        pointHoverRadius: 5,
        yAxisID: 'y1',
      }
    ]
  }
})

const chartOptions = {
  responsive: true,
  maintainAspectRatio: false,
  interaction: { mode: 'index' as const, intersect: false },
  scales: {
    y: {
      type: 'linear' as const,
      display: true,
      position: 'left' as const,
      title: { display: true, text: t('payment.admin.revenue') },
    },
    y1: {
      type: 'linear' as const,
      display: true,
      position: 'right' as const,
      title: { display: true, text: t('payment.admin.orderCount') },
      grid: { drawOnChartArea: false },
    }
  },
  plugins: {
    legend: { position: 'top' as const },
  }
}
</script>
