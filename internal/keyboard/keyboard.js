// Page-side half of the keyboard audit, installed once as window.__ffkb.
// Element identity crosses the CDP boundary as a numeric id from a WeakMap,
// so nothing is written into the app's DOM.
(() => {
  if (window.__ffkb) return;

  const ids = new WeakMap();
  const byID = new Map();
  let nextID = 1;
  const idOf = (el) => {
    if (!ids.has(el)) {
      ids.set(el, nextID);
      byID.set(nextID++, new WeakRef(el));
    }
    return ids.get(el);
  };

  const FOCUSABLE = [
    'a[href]', 'area[href]', 'button', 'input:not([type="hidden"])', 'select',
    'textarea', 'iframe', 'summary', '[tabindex]', 'audio[controls]',
    'video[controls]', '[contenteditable]:not([contenteditable="false"])',
  ].join(',');

  // Mirrors the fingerprinter's generated-id test: anchoring a selector on a
  // per-session id would make every run look like a new violation.
  const generatedID = /[0-9a-f]{8,}|[0-9]{4,}$|^:r[a-z0-9]+:?$/;

  function selectorOf(el) {
    const parts = [];
    for (let n = el; n && n.nodeType === 1 && n !== document.documentElement; n = n.parentElement) {
      if (n.id && !generatedID.test(n.id)) {
        parts.unshift('#' + CSS.escape(n.id));
        break;
      }
      // Focus-state classes (Mui-focused, Mui-focusVisible) are dropped: the
      // selector is taken while the element has focus but must still match,
      // and fingerprint the same, once it has lost it.
      const classes = [...n.classList].filter((c) => !/focus/i.test(c)).slice(0, 3);
      let seg = n.localName + classes.map((c) => '.' + CSS.escape(c)).join('');
      const parent = n.parentElement;
      if (parent && [...parent.children].filter((c) => c.matches(seg)).length > 1) {
        seg += `:nth-child(${[...parent.children].indexOf(n) + 1})`;
      }
      parts.unshift(seg);
    }
    return parts.join(' > ');
  }

  function snippet(el) {
    const html = el.outerHTML;
    return html.length <= 250 ? html : el.cloneNode(false).outerHTML;
  }

  function deepActive() {
    let a = document.activeElement;
    while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
    return a;
  }

  function containsDeep(root, el) {
    for (let n = el; n; n = n.getRootNode().host) {
      if (root.contains(n)) return true;
    }
    return false;
  }

  // Native radios share one tab stop per group; reaching any member reaches
  // the group.
  function radioGroup(el) {
    if (el.localName !== 'input' || el.type !== 'radio' || !el.name) return '';
    return (el.form ? 'f' + idOf(el.form) : 'd') + ':' + el.name;
  }

  // A 0x0 focusable with no sized descendants is a focus guard (MUI, Radix,
  // focus-trap sentinels redirect focus the moment they receive it), not a
  // control anyone is meant to reach.
  function hasSize(el) {
    const r = el.getBoundingClientRect();
    if (r.width > 1 && r.height > 1) return true;
    for (const d of el.querySelectorAll('*')) {
      const dr = d.getBoundingClientRect();
      if (dr.width > 1 && dr.height > 1) return true;
    }
    return false;
  }

  function hiddenReason(el) {
    if (!hasSize(el)) return 'it renders at 1px or smaller';
    const cs = getComputedStyle(el);
    if (cs.clip === 'rect(0px, 0px, 0px, 0px)' || cs.clipPath === 'inset(50%)') {
      return 'it is clipped out of view';
    }
    const r = el.getBoundingClientRect();
    if (r.right + scrollX <= 0 || r.bottom + scrollY <= 0) return 'it is positioned off-screen';
    return '';
  }

  const shown = (el) => el.checkVisibility({ visibilityProperty: true, opacityProperty: true });

  // An explicit modal (aria-modal, an open modal <dialog>), else a body child
  // whose siblings are all aria-hidden or inert while they still show
  // focusable content - how portal modals (MUI, Radix) wall off the page.
  // Requiring visible focusables behind it keeps an aria-hidden sprite sheet,
  // or a closed menu kept mounted beside the app root, from reading as the
  // page behind a modal.
  function findModal() {
    const explicit = [...document.querySelectorAll('dialog, [aria-modal="true"]')].filter(
      (d) => (d.localName !== 'dialog' || d.matches(':modal')) && shown(d),
    );
    if (explicit.length) return explicit[explicit.length - 1];
    const kids = [...document.body.children].filter(
      (k) => !['script', 'style', 'template', 'link', 'noscript'].includes(k.localName),
    );
    const walled = (k) => k.getAttribute('aria-hidden') === 'true' || k.inert;
    const open = kids.filter((k) => !walled(k));
    const behind = kids.filter(walled);
    if (open.length === 1 && shown(open[0]) && behind.some((k) => [...k.querySelectorAll(FOCUSABLE)].some(shown))) {
      return open[0];
    }
    return null;
  }

  // The region a focus indicator is looked for in: the element plus the
  // close-fitting wrappers around it, since MUI and most design systems
  // draw an input's ring on its root, past the <input> or <textarea> itself.
  function focusBox(el) {
    const r = el.getBoundingClientRect();
    let [l, t, rt, b] = [r.left, r.top, r.right, r.bottom];
    for (let n = el.parentElement, i = 0; n && n !== document.body && i < 3; n = n.parentElement, i++) {
      const p = n.getBoundingClientRect();
      if (l - p.left > 32 || t - p.top > 32 || p.right - rt > 32 || p.bottom - b > 32) break;
      [l, t, rt, b] = [Math.min(l, p.left), Math.min(t, p.top), Math.max(rt, p.right), Math.max(b, p.bottom)];
    }
    return [l + scrollX, t + scrollY, rt - l, b - t];
  }

  let modal = null;
  let scopeEl = null;
  let style = null;
  let start = null;
  let scroll0 = [0, 0];

  const inScope = (el) => !scopeEl || containsDeep(scopeEl, el);

  window.__ffkb = {
    init(scope) {
      scopeEl = scope ? document.querySelector(scope) : null;
      if (scope && !scopeEl) throw new Error(`scope ${JSON.stringify(scope)} matches no element`);
      // A blinking caret would make an input with no focus style look styled.
      style = document.createElement('style');
      style.textContent = '*, *::before, *::after { caret-color: transparent !important; }';
      document.head.appendChild(style);
      modal = findModal();
      start = deepActive();
      scroll0 = [scrollX, scrollY];
      return {
        modal: modal && { sel: selectorOf(modal), html: snippet(modal) },
        activeInModal: !modal || (!!start && containsDeep(modal, start)),
      };
    },

    // Tabbable elements, within the open modal when there is one and the
    // scan scope, that no Tab press reached. A stop counts as reached when
    // focus landed on it or inside it: MUI's DataGrid header hands focus
    // straight to its checkbox, which is the control the user needs.
    unreached(reachedIDs) {
      const reached = reachedIDs.map((id) => byID.get(id)?.deref()).filter(Boolean);
      const groups = new Set(reached.map(radioGroup).filter(Boolean));
      const root = modal || document;
      return [...root.querySelectorAll(FOCUSABLE)]
        .filter((el) => inScope(el) && el.tabIndex >= 0 && !el.matches(':disabled') &&
          !el.closest('[inert]') && el.checkVisibility({ visibilityProperty: true }) && hasSize(el))
        .filter((el) => !reached.some((r) => containsDeep(el, r)) && !groups.has(radioGroup(el)))
        .map((el) => ({ sel: selectorOf(el), html: snippet(el) }));
    },

    // How many tab stops the open modal holds, so a lone stop that Tab
    // cannot leave reads as containment rather than a trap.
    modalStops() {
      if (!modal) return 0;
      return [...modal.querySelectorAll(FOCUSABLE)].filter((el) => el.tabIndex >= 0 && hasSize(el)).length;
    },

    // Gives the effects a focus change started time to paint. Frameworks
    // mount them from a state update after the focus event (MUI adds a
    // class, then inserts its ripple a frame later), so first wait for a
    // frame with no DOM changes, up to 5. Then let the transitions and
    // animations that started run briefly: regions are compared pixel for
    // pixel, so an indicator need only have visibly begun, and the cap keeps
    // MUI's 200ms ripple, on nearly every stop, from dominating a traversal.
    // Infinite animations (the pulsating focus ripple) are not waited on.
    async settle(extraMs = 0) {
      let mutated = true;
      const mo = new MutationObserver(() => { mutated = true; });
      mo.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
      for (let i = 0; i < 5 && mutated; i++) {
        mutated = false;
        await new Promise((r) => requestAnimationFrame(r));
      }
      mo.disconnect();
      const finite = document.getAnimations().filter((a) => {
        const t = a.effect && a.effect.getComputedTiming();
        return a.playState === 'running' && t && Number.isFinite(t.endTime);
      });
      await Promise.race([
        Promise.all(finite.map((a) => a.finished.catch(() => {}))),
        new Promise((r) => setTimeout(r, 60)),
      ]);
      if (extraMs) await new Promise((r) => setTimeout(r, extraMs));
    },

    state() {
      const view = [scrollX, scrollY, innerWidth, innerHeight];
      const a = deepActive();
      if (!document.hasFocus() || !a || a === document.body || a === document.documentElement) {
        return { out: true, view };
      }
      return {
        out: false,
        view,
        id: idOf(a),
        frame: a.localName === 'iframe',
        sel: selectorOf(a),
        html: snippet(a),
        hidden: hiddenReason(a),
        inModal: !modal || containsDeep(modal, a),
        inScope: inScope(a),
        box: focusBox(a),
      };
    },

    // Puts focus and scroll back where the audit found them, so steps after
    // the audit run against the state the config reached.
    finish() {
      if (style) style.remove();
      if (start && start.isConnected && start !== document.body) start.focus({ preventScroll: true });
      else if (document.activeElement) document.activeElement.blur();
      scrollTo(scroll0[0], scroll0[1]);
    },
  };
})();
