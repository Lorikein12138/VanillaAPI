<template>
  <canvas ref="canvasRef" class="auth-silk" :class="{ 'is-ready': ready }" aria-hidden="true"></canvas>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'

// 奶油丝绸：与首页同一套着色器。低分辨率画布上算多层扭曲噪声并打光，CSS 拉伸铺满；
// 画面底色取页面底色，靠外层遮罩渐隐进页面，不出现面板边界。
// 25fps、离开视口或标签页隐藏时暂停；减少动态效果时只画一帧。
// 移动端片元常是 mediump(fp16)：格点哈希查 16×16 噪声纹理、时间位移由 JS 按八度折回 ±8，
// 着色器里的数值始终很小，不会因精度不足碎成马赛克。

const props = defineProps<{ dark: boolean }>()

const canvasRef = ref<HTMLCanvasElement | null>(null)
const ready = ref(false)

const VS = 'attribute vec2 p;void main(){gl_Position=vec4(p,0.,1.);}'
const FS = [
  '#ifdef GL_FRAGMENT_PRECISION_HIGH',
  'precision highp float;',
  '#else',
  'precision mediump float;',
  '#endif',
  'uniform vec2 R;uniform vec2 P;uniform float S;uniform vec2 O[9];uniform sampler2D N;',
  'uniform vec3 cD,cM,cH,cA;uniform float kA,kH;',
  'float hs(vec2 i){return texture2D(N,(i+.5)/16.).r;}',
  'float ns(vec2 p){vec2 i=floor(p),f=fract(p),u=f*f*(3.-2.*f);',
  ' return mix(mix(hs(i),hs(i+vec2(1,0)),u.x),mix(hs(i+vec2(0,1)),hs(i+vec2(1,1)),u.x),u.y);}',
  'const mat2 K=mat2(1.624,1.218,-1.218,1.624);',
  'float fb(vec2 p,vec2 a,vec2 b,vec2 c){vec2 q=K*p;return .5*ns(p+a)+.21*ns(q+b)+.0882*ns(K*q+c);}',
  'vec2 Q;',
  'float H(vec2 p){',
  ' Q=vec2(fb(p,O[0],O[1],O[2]),fb(p+vec2(5.2,1.3),O[3],O[4],O[5]));',
  ' return fb(p+2.4*Q+P,O[6],O[7],O[8]);}',
  'void main(){',
  ' vec2 uv=gl_FragCoord.xy/R.y;vec2 p=vec2(uv.x*.7,uv.y*1.6)*S;',
  ' float e=3./R.y*S;float h=H(p);vec2 q=Q;',
  ' float hx=H(p+vec2(e,0.)),hy=H(p+vec2(0.,e));',
  ' vec3 n=normalize(vec3((h-hx)/e,(h-hy)/e,1.6));',
  ' vec3 L=normalize(vec3(-.45,.6,.65));',
  ' float d=clamp(dot(n,L),0.,1.);',
  ' float sp=pow(clamp(dot(reflect(-L,n),vec3(0,0,1)),0.,1.),14.);',
  ' vec3 c=mix(cD,cM,smoothstep(.35,.95,d));',
  ' c=mix(c,cA,kA*smoothstep(.45,.85,q.y)*smoothstep(.5,.2,d));',
  ' c+=cH*sp*kH;',
  ' gl_FragColor=vec4(c,1.);}'
].join('\n')

interface Palette {
  cD: string
  cM: string
  cH: string
  cA: string
  kA: number
  kH: number
}

// 中间调 cM 贴近页面底色（浅 #FAF9F5 / 深 #262624），渐隐处与页面无缝衔接
const PALETTES: Record<'light' | 'dark', Palette> = {
  light: { cD: '#DCCCB4', cM: '#F8F4EB', cH: '#FFFFFF', cA: '#D4A27F', kA: 0.85, kH: 0.7 },
  dark: { cD: '#1E1D1B', cM: '#35322D', cH: '#8F735C', cA: '#7E4A34', kA: 0.65, kH: 0.55 }
}

const SCALE = 1.15
const SPEED = 0.002
const RES = 0.5

function hex3(h: string): [number, number, number] {
  const n = parseInt(h.slice(1), 16)
  return [((n >> 16) & 255) / 255, ((n >> 8) & 255) / 255, (n & 255) / 255]
}

function fr(x: number): number {
  return x - Math.floor(x)
}

const NOISE = (() => {
  const a = new Uint8Array(256)
  for (let y = 0; y < 16; y++) {
    for (let x = 0; x < 16; x++) {
      const u = fr(x * 123.34)
      const v = fr(y * 456.21)
      const d = u * (u + 45.32) + v * (v + 45.32)
      a[y * 16 + x] = Math.floor(fr((u + d) * (v + d)) * 256)
    }
  }
  return a
})()

// 噪声以 16 为周期，各八度的时间位移 K^k·off 折回 [-8,8) 后画面不变。
function offsets(t: number, out: Float32Array): Float32Array {
  let n = 0
  const seeds: Array<[number, number]> = [
    [0, t * 0.05],
    [-t * 0.04, 0],
    [t * 0.03, -t * 0.02]
  ]
  for (let v of seeds) {
    for (let k = 0; k < 3; k++) {
      out[n++] = v[0] - 16 * Math.floor(v[0] / 16 + 0.5)
      out[n++] = v[1] - 16 * Math.floor(v[1] / 16 + 0.5)
      v = [2.03 * (0.8 * v[0] - 0.6 * v[1]), 2.03 * (0.6 * v[0] + 0.8 * v[1])]
    }
  }
  return out
}

let cleanup: (() => void) | null = null
let repaint: (() => void) | null = null

onMounted(() => {
  const canvas = canvasRef.value
  if (!canvas) return
  const gl = canvas.getContext('webgl', {
    alpha: false,
    antialias: false,
    depth: false,
    stencil: false,
    premultipliedAlpha: false,
    powerPreference: 'low-power'
  })
  if (!gl) return

  const shader = (type: number, src: string) => {
    const s = gl.createShader(type)!
    gl.shaderSource(s, src)
    gl.compileShader(s)
    return s
  }
  const program = gl.createProgram()!
  gl.attachShader(program, shader(gl.VERTEX_SHADER, VS))
  gl.attachShader(program, shader(gl.FRAGMENT_SHADER, FS))
  gl.linkProgram(program)
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return
  gl.useProgram(program)

  gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer())
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW)
  const loc = gl.getAttribLocation(program, 'p')
  gl.enableVertexAttribArray(loc)
  gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0)

  const U: Record<string, WebGLUniformLocation | null> = {}
  for (const k of ['R', 'P', 'S', 'O', 'N', 'cD', 'cM', 'cH', 'cA', 'kA', 'kH']) {
    U[k] = gl.getUniformLocation(program, k)
  }

  gl.activeTexture(gl.TEXTURE0)
  gl.bindTexture(gl.TEXTURE_2D, gl.createTexture())
  gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1)
  gl.texImage2D(gl.TEXTURE_2D, 0, gl.LUMINANCE, 16, 16, 0, gl.LUMINANCE, gl.UNSIGNED_BYTE, NOISE)
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST)
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST)
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.REPEAT)
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.REPEAT)
  gl.uniform1i(U.N, 0)

  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  const finePointer = window.matchMedia('(pointer: fine)').matches
  const of = new Float32Array(18)
  const m = { x: 0, y: 0, tx: 0, ty: 0 }
  let t = 3
  let raf = 0
  let last = 0
  let visible = false
  let lost = false

  const palette = () => {
    const c = PALETTES[props.dark ? 'dark' : 'light']
    gl.uniform3fv(U.cD, hex3(c.cD))
    gl.uniform3fv(U.cM, hex3(c.cM))
    gl.uniform3fv(U.cH, hex3(c.cH))
    gl.uniform3fv(U.cA, hex3(c.cA))
    gl.uniform1f(U.kA, c.kA)
    gl.uniform1f(U.kH, c.kH)
  }
  const draw = () => {
    if (lost || !canvas.width) return
    gl.uniform2fv(U.O, offsets(t, of))
    gl.uniform2f(U.P, m.x, m.y)
    gl.drawArrays(gl.TRIANGLES, 0, 3)
    ready.value = true
  }
  const resize = () => {
    const r = canvas.getBoundingClientRect()
    if (!r.width || !r.height) return
    canvas.width = Math.max(2, Math.round(r.width * RES))
    canvas.height = Math.max(2, Math.round(r.height * RES))
    gl.viewport(0, 0, canvas.width, canvas.height)
    gl.uniform2f(U.R, canvas.width, canvas.height)
    // 纹理尺度跟视口高度走：窄屏只铺顶部一截时，纹理疏密与整屏一致
    gl.uniform1f(U.S, (SCALE * Math.max(r.height, window.innerHeight)) / 700)
    draw()
  }
  const running = () => visible && !lost && document.visibilityState === 'visible'
  const loop = (now: number) => {
    if (!running()) {
      raf = 0
      return
    }
    raf = requestAnimationFrame(loop)
    if (now - last < 40) return
    const dt = Math.min(now - last, 80)
    last = now
    t += dt * SPEED
    m.x += (m.tx - m.x) * 0.03
    m.y += (m.ty - m.y) * 0.03
    draw()
  }
  const kick = () => {
    if (reduce || raf || !running()) return
    last = performance.now()
    raf = requestAnimationFrame(loop)
  }

  repaint = () => {
    palette()
    draw()
  }
  palette()
  resize()

  const ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(resize) : null
  if (ro) ro.observe(canvas)
  else window.addEventListener('resize', resize)

  const io = new IntersectionObserver((entries) => {
    visible = entries[0].isIntersecting
    kick()
  })
  io.observe(canvas)

  const onVisibility = () => kick()
  document.addEventListener('visibilitychange', onVisibility)

  const onLost = (e: Event) => {
    e.preventDefault()
    lost = true
  }
  canvas.addEventListener('webglcontextlost', onLost)

  const onPointer = (e: PointerEvent) => {
    m.tx = (e.clientX / window.innerWidth - 0.5) * 0.6
    m.ty = (e.clientY / window.innerHeight - 0.5) * -0.6
  }
  if (finePointer && !reduce) window.addEventListener('pointermove', onPointer, { passive: true })

  cleanup = () => {
    if (raf) cancelAnimationFrame(raf)
    raf = 0
    visible = false
    io.disconnect()
    if (ro) ro.disconnect()
    else window.removeEventListener('resize', resize)
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('pointermove', onPointer)
    canvas.removeEventListener('webglcontextlost', onLost)
    gl.getExtension('WEBGL_lose_context')?.loseContext()
  }
})

watch(
  () => props.dark,
  () => repaint?.()
)

onBeforeUnmount(() => {
  cleanup?.()
  cleanup = null
  repaint = null
})
</script>

<style scoped>
.auth-silk {
  display: block;
  width: 100%;
  height: 100%;
  opacity: 0;
  transition: opacity 2.2s cubic-bezier(0.22, 1, 0.36, 1);
}

.auth-silk.is-ready {
  opacity: 1;
}

@media (prefers-reduced-motion: reduce) {
  .auth-silk {
    transition: none;
  }
}
</style>
