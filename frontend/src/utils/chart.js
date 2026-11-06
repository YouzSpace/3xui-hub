/**
 * SVG 折线几何 —— 只算坐标，不管样式。
 *
 * 颜色 / 字号 / 网格由调用方（组件）用自己的 token 决定，所以这里只输出
 * path 字符串和一对坐标换算函数。
 *
 * 曲线**永远视觉连续**：数据缺口（页面隐藏 / 切页 / 轮询失败导致采样间隔变大）
 * 与缺失值（null / 负数）都用直线连接，不再断开 —— 断口在普通用户眼里像故障。
 * 缺失值只跳过该点、不打断连线；无采样区间按两端有效点插值成一条直线，
 * 这是刻意的产品取舍（宁可画一条直线，也不要一个看着像出问题的豁口）。
 * 依然不把缺失值画成 0：跳过的点不参与画线，不会掉到 0。
 */

/**
 * @param {Array<{t:number}>} samples 采样点，按时间升序
 * @param {Array<{key:string,axis?:1|2}>} series 要画的系列（取 sample[key] 作为值；
 *        axis=2 的系列走右轴量程，默认 1）
 * @param {object} options width / height / padTop / padBottom / padLeft / padRight / max / max2
 * @returns {{start:number,end:number,ceiling:number,ceiling2:number,x:Function,y:Function,
 *            y2:Function,paths:Array<{key:string,d:string}>,count:number}}
 */
export function chartGeometry(samples, series, options = {}) {
  const {
    width = 600,
    height = 160,
    padLeft = 0,
    padRight = 0,
    padTop = 12,
    padBottom = 12,
    max = null,
    max2 = null,
  } = options

  const points = (samples || []).filter((sample) => Number.isFinite(sample?.t))
  const start = points.length ? points[0].t : 0
  const end = points.length ? points[points.length - 1].t : start + 1
  const span = Math.max(1, end - start)

  const list = series || []
  const axisOf = (s) => (s?.axis === 2 ? 2 : 1)

  // 两条轴各自量程：速率（B/s）与累计量（字节）数量级差几个数量级，
  // 共用一条轴的话累计线会被压成贴顶的直线、速率线贴底。
  const valuesOn = (axis) =>
    points
      .flatMap((point) => list.filter((s) => axisOf(s) === axis).map((s) => point[s.key]))
      .filter((value) => Number.isFinite(value) && value >= 0)

  const ceiling = max ?? Math.max(1, ...valuesOn(1)) * 1.15
  const ceiling2 = max2 ?? Math.max(1, ...valuesOn(2)) * 1.15

  const plotWidth = Math.max(1, width - padLeft - padRight)
  const plotHeight = Math.max(1, height - padTop - padBottom)

  const x = (t) => padLeft + ((t - start) / span) * plotWidth
  const y = (value) => padTop + (1 - Math.min(value, ceiling) / ceiling) * plotHeight
  const y2 = (value) => padTop + (1 - Math.min(value, ceiling2) / ceiling2) * plotHeight

  const paths = list.map((s) => {
    const toY = axisOf(s) === 2 ? y2 : y
    let d = ''
    let previous = null

    for (const point of points) {
      const value = point[s.key]
      // 缺失值只跳过该点，保留连线状态 —— 前后有效点直连，不留断口
      if (!Number.isFinite(value) || value < 0) continue
      const command = previous === null ? 'M' : 'L'
      d += `${command}${x(point.t).toFixed(2)},${toY(value).toFixed(2)}`
      previous = point
    }

    return { key: s.key, d }
  })

  return {
    start,
    end,
    ceiling,
    ceiling2,
    x,
    y,
    y2,
    paths,
    // areaPath：折线下方的填充区域（先沿折线走一遍，再闭合到底边）。
    // 没有 path 时返回空串。铺满式监控卡用：折线 + 渐变面积两层。
    areaPath: paths.map((p) => {
      if (!p.d) return ''
      // 末点 x + 底边 y(height) 闭合：M..L..L(x_last,height)L(x_first,height)Z
      const coords = p.d.match(/[ML]([\d.]+),([\d.]+)/g) || []
      if (coords.length < 2) return ''
      const first = coords[0].slice(1).split(',')
      const last = coords[coords.length - 1].slice(1).split(',')
      return `${p.d}L${last[0]},${height}L${first[0]},${height}Z`
    }),
    width,
    height,
    padTop,
    plotHeight,
    padLeft,
    plotWidth,
    count: points.length,
  }
}
