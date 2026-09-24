import { useEffect } from 'react'

// 折叠图标条（rail）tooltip 定位：.left / .right 恒定 overflow:hidden，且宽度 = 图标条宽（44px），
// 绝对定位的伪元素 tooltip 会在图标条边界被整条裁掉（实测既有 5 个按钮的提示全部不可见）。
// 解法：伪元素改 position:fixed —— fixed 的包含块是视口，不受祖先 overflow 裁剪（图标条祖先链
// 无 transform/filter/contain，不会把 fixed 拉回局部坐标系）；坐标在 hover 时写入 CSS 变量
// （fixed 无法自行跟随元素位置）。
//
// 委托安装一次（document 级 mouseover）：图标条随折叠态挂载/卸载，绑到元素上会随生命周期失效；
// 一个监听同时覆盖左右两条 rail，按所属面板决定展开方向（左栏向右、右栏向左，均不越窗口边界）。
export function useRailTips(): void {
  useEffect(() => {
    // 元素常驻（rail 仅在折叠态渲染，但复用同一批 DOM 时变量会残留）→ 每次 hover 前清掉另一侧，
    // 避免「左栏写 --tip-x、随后右栏 hover 未清 → left/right 同时生效」导致定位错乱。
    const place = (btn: HTMLElement, nearRightEdge: boolean) => {
      const r = btn.getBoundingClientRect()
      if (!r.width) return false // 未布局（隐藏态）
      const style = btn.style
      style.setProperty('--tip-y', `${Math.round(r.top + r.height / 2)}px`)
      if (nearRightEdge) {
        style.removeProperty('--tip-x')
        style.setProperty('--tip-rx', `${Math.round(window.innerWidth - r.left + 8)}px`)
      } else {
        style.removeProperty('--tip-rx')
        style.setProperty('--tip-x', `${Math.round(r.right + 8)}px`)
      }
      return true
    }
    const onOver = (e: Event) => {
      const btn = (e.target as Element | null)?.closest?.('.rail .rbtn[data-tip]') as HTMLElement | null
      if (!btn) return
      place(btn, !!btn.closest('.right'))
    }
    // 键盘聚焦也要能看到提示（无鼠标时 aria-label 不可见）
    const onFocus = (e: Event) => {
      const btn = (e.target as Element | null)?.closest?.('.rail .rbtn[data-tip]') as HTMLElement | null
      if (btn) place(btn, !!btn.closest('.right'))
    }
    document.addEventListener('mouseover', onOver)
    document.addEventListener('focusin', onFocus)
    return () => {
      document.removeEventListener('mouseover', onOver)
      document.removeEventListener('focusin', onFocus)
    }
  }, [])
}
