// Tiny DOM builder: h('div', {class: 'card', onclick: fn}, child, …).
// Strings become text nodes; arrays flatten; null/undefined are skipped.

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null) continue;
    if (k.startsWith('on') && typeof v === 'function') {
      el.addEventListener(k.slice(2), v);
    } else if (k === 'dataset') {
      Object.assign(el.dataset, v);
    } else if (k === 'style' && typeof v === 'object') {
      for (const [prop, val] of Object.entries(v)) {
        if (prop.startsWith('--')) el.style.setProperty(prop, val);
        else el.style[prop] = val;
      }
    } else if (k in el && k !== 'type' && k !== 'value') {
      try { el[k] = v; } catch { el.setAttribute(k, v); }
    } else {
      el.setAttribute(k, v);
    }
  }
  append(el, children);
  return el;
}

function append(el, kids) {
  for (const k of kids) {
    if (k == null) continue;
    if (Array.isArray(k)) { append(el, k); continue; }
    el.append(k.nodeType ? k : document.createTextNode(String(k)));
  }
}
