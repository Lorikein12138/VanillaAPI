package service

import (
	"math"
	"time"
)

// 响应级展示数据里“缓存率”和“每秒 Token”都不是每个 5 分钟原子各自独立的随机数，
// 而是每个叶子（平台 × 分组 × 模型）各自缓慢漂移的一条曲线：相邻原子接近，隔一阵子
// 才会有一段明显的起伏，不同叶子的起伏时刻与幅度互不相同。曲线只由叶子与原子起点决定，
// 与时间窗、筛选、缩放无关，所以窗口重叠、重复刷新时同一原子的数值恒定。
const (
	channelMonitorV2SyntheticCacheMin = .80
	channelMonitorV2SyntheticCacheMax = .99
	channelMonitorV2SyntheticTPSMin   = 10.0
	channelMonitorV2SyntheticTPSMax   = 50.0

	// 每个原子的 token 总量 = 每秒 Token × 原子秒数；展示的每秒 Token 是 TPM ÷ 60，
	// 所以 5 分钟原子折算回去正好是这里取的值。
	channelMonitorV2SyntheticAtomSeconds = 300
	// 同一平台下模型很多时叶子吞吐会被摊薄，token 总量不低于此值，以免取整后缓存分子分母失真。
	channelMonitorV2SyntheticMinAtomTokens = 120

	// 缓存读取占缓存分母（输入 + 缓存写入 + 缓存读取）的百分比范围。
	channelMonitorV2SyntheticCachePercentMin int64 = 80
	channelMonitorV2SyntheticCachePercentMax int64 = 99

	// 一个“节”内曲线从上一个节点值走向下一个节点值；缓存率变得更慢。
	// 相邻两个 5 分钟原子之间曲线本身的变化量上限（不含抖动）：
	// 每秒 Token ≤ 1.5×40÷(0.36×36)≈4.6，缓存率 ≤ 1.5×19pp÷(0.36×72)≈1.1pp。
	channelMonitorV2SyntheticCacheCell = 6 * time.Hour
	channelMonitorV2SyntheticTPSCell   = 3 * time.Hour

	// 在曲线之上叠加的原子级小抖动幅度（峰峰值）。
	channelMonitorV2SyntheticCacheJitter = .012
	channelMonitorV2SyntheticTPSJitter   = 3.0
)

const (
	channelMonitorV2SyntheticSaltCache  uint64 = 0x43414348455f5631
	channelMonitorV2SyntheticSaltTPS    uint64 = 0x5450535f5f5f5631
	channelMonitorV2SyntheticSaltPhase  uint64 = 0x70686173655f7631
	channelMonitorV2SyntheticSaltShape  uint64 = 0x73686170655f7631
	channelMonitorV2SyntheticSaltJitter uint64 = 0x6a69747465725f31
)

// channelMonitorV2SyntheticLeafSeed hashes the leaf identity (FNV-1a) without
// allocating; the result is only a seed for the splitmix mixer below.
func channelMonitorV2SyntheticLeafSeed(platform string, groupID int64, model string) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	const separator uint64 = 0xff
	hash := offset
	for index := 0; index < len(platform); index++ {
		hash ^= uint64(platform[index])
		hash *= prime
	}
	hash ^= separator
	hash *= prime
	for shift := 0; shift < 64; shift += 8 {
		hash ^= uint64(groupID>>uint(shift)) & 0xff
		hash *= prime
	}
	hash ^= separator
	hash *= prime
	for index := 0; index < len(model); index++ {
		hash ^= uint64(model[index])
		hash *= prime
	}
	return channelMonitorV2SyntheticSplitMix(hash)
}

func channelMonitorV2SyntheticSplitMix(value uint64) uint64 {
	value += 0x9e3779b97f4a7c15
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func channelMonitorV2SyntheticMix(seed, salt uint64, index int64) uint64 {
	return channelMonitorV2SyntheticSplitMix(channelMonitorV2SyntheticSplitMix(seed^salt) + uint64(index))
}

// channelMonitorV2SyntheticUnit maps 64 random bits to [0, 1).
func channelMonitorV2SyntheticUnit(value uint64) float64 {
	return float64(value>>11) / (1 << 53)
}

// channelMonitorV2SyntheticDrift returns a slowly wandering value in [0, 1].
//
// Time is cut into cells of length cell (shifted by a per-leaf phase so leaves do
// not turn at the same instant). Every cell boundary owns a random node value;
// inside a cell the curve stays at the left node for a random hold fraction, then
// eases to the right node over a random share of the remaining time. The curve is
// continuous at cell boundaries and never leaves [0, 1].
func channelMonitorV2SyntheticDrift(seed, salt uint64, at time.Time, cell time.Duration) float64 {
	cellSeconds := int64(cell / time.Second)
	phase := int64(channelMonitorV2SyntheticMix(seed, salt^channelMonitorV2SyntheticSaltPhase, 0) % uint64(cellSeconds))
	shifted := at.UTC().Unix() + phase
	index := shifted / cellSeconds
	offset := shifted % cellSeconds
	if offset < 0 {
		index--
		offset += cellSeconds
	}
	from := channelMonitorV2SyntheticUnit(channelMonitorV2SyntheticMix(seed, salt, index))
	to := channelMonitorV2SyntheticUnit(channelMonitorV2SyntheticMix(seed, salt, index+1))
	shape := channelMonitorV2SyntheticMix(seed, salt^channelMonitorV2SyntheticSaltShape, index)
	hold := float64(shape&0xffffffff) / (1 << 32) * .4
	span := (1 - hold) * (.6 + .4*float64(shape>>32)/(1<<32))
	progress := (float64(offset)/float64(cellSeconds) - hold) / span
	switch {
	case progress <= 0:
		progress = 0
	case progress >= 1:
		progress = 1
	}
	eased := progress * progress * (3 - 2*progress)
	return from + (to-from)*eased
}

func channelMonitorV2SyntheticClamp(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// channelMonitorV2SyntheticCacheRate is the leaf's cache-read share at one atom, in [80%, 99%].
func channelMonitorV2SyntheticCacheRate(seed uint64, atom time.Time) float64 {
	base := channelMonitorV2SyntheticDrift(seed, channelMonitorV2SyntheticSaltCache, atom, channelMonitorV2SyntheticCacheCell)
	rate := channelMonitorV2SyntheticCacheMin + (channelMonitorV2SyntheticCacheMax-channelMonitorV2SyntheticCacheMin)*base
	jitter := channelMonitorV2SyntheticUnit(channelMonitorV2SyntheticMix(seed, channelMonitorV2SyntheticSaltCache^channelMonitorV2SyntheticSaltJitter, atom.UTC().Unix())) - .5
	rate += jitter * channelMonitorV2SyntheticCacheJitter
	return channelMonitorV2SyntheticClamp(rate, channelMonitorV2SyntheticCacheMin, channelMonitorV2SyntheticCacheMax)
}

// channelMonitorV2SyntheticCacheReadTokens rounds the cache rate into whole tokens
// and keeps the realised share (cacheRead ÷ denominator) inside [80%, 99%] with
// integer arithmetic, so no float edge case can push it to 100% or below 80%.
func channelMonitorV2SyntheticCacheReadTokens(denominator int64, rate float64) int64 {
	low := (denominator*channelMonitorV2SyntheticCachePercentMin + 99) / 100
	high := denominator * channelMonitorV2SyntheticCachePercentMax / 100
	value := int64(math.Round(float64(denominator) * rate))
	if value > high {
		value = high
	}
	if value < low {
		value = low
	}
	return value
}

// channelMonitorV2SyntheticTokensPerSecond is the leaf's throughput at one atom, in [10, 50].
func channelMonitorV2SyntheticTokensPerSecond(seed uint64, atom time.Time) float64 {
	base := channelMonitorV2SyntheticDrift(seed, channelMonitorV2SyntheticSaltTPS, atom, channelMonitorV2SyntheticTPSCell)
	rate := channelMonitorV2SyntheticTPSMin + (channelMonitorV2SyntheticTPSMax-channelMonitorV2SyntheticTPSMin)*base
	jitter := channelMonitorV2SyntheticUnit(channelMonitorV2SyntheticMix(seed, channelMonitorV2SyntheticSaltTPS^channelMonitorV2SyntheticSaltJitter, atom.UTC().Unix())) - .5
	rate += jitter * channelMonitorV2SyntheticTPSJitter
	return channelMonitorV2SyntheticClamp(rate, channelMonitorV2SyntheticTPSMin, channelMonitorV2SyntheticTPSMax)
}
