import { useEffect, useState } from 'react'

/* The narrative skin lives in one place: the `data-skin` attribute that
   lib/narrativeSkin.ts writes from settings. Components that have to change
   *structure* (not just style) read it here rather than from the store, so the
   markup and the CSS can never disagree — a styled-with-runtime / rendered-as-
   current mismatch is exactly the class of bug that looks like "the skin did
   nothing". */
export type Skin = 'current' | 'v3' | 'runtime'

function read(): Skin {
  const v = typeof document !== 'undefined' ? document.documentElement.dataset.skin : ''
  return v === 'v3' || v === 'runtime' ? v : 'current'
}

export function useSkin(): Skin {
  const [skin, setSkin] = useState<Skin>(read)
  useEffect(() => {
    const el = document.documentElement
    const sync = () => setSkin(read())
    sync()
    const obs = new MutationObserver(sync)
    obs.observe(el, { attributes: true, attributeFilter: ['data-skin'] })
    return () => obs.disconnect()
  }, [])
  return skin
}
