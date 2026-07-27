// A small, dependency-free markdown renderer for Ink. Agent replies are
// markdown; this turns them into styled terminal blocks — fenced code in a
// bordered box, headers, bullet/numbered lists, blockquotes, and inline
// **bold** / *italic* / `code`. Not a full CommonMark parser: just the elements
// agents actually produce, rendered to look good in a terminal.

import React from "react";
import { Box, Text } from "ink";

const h = React.createElement;

// Inline spans: **bold**, `code`, *italic*, __bold__. Returns Text children.
function inline(text, C) {
  const spans = [];
  const re = /(\*\*([^*]+)\*\*|__([^_]+)__|`([^`]+)`|\*([^*]+)\*)/g;
  let last = 0, m, k = 0;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) spans.push(h(Text, { key: k++ }, text.slice(last, m.index)));
    if (m[2] != null) spans.push(h(Text, { key: k++, bold: true, color: C.text }, m[2]));
    else if (m[3] != null) spans.push(h(Text, { key: k++, bold: true, color: C.text }, m[3]));
    else if (m[4] != null) spans.push(h(Text, { key: k++, color: C.green, backgroundColor: "#1b2130" }, ` ${m[4]} `));
    else if (m[5] != null) spans.push(h(Text, { key: k++, italic: true, color: C.blue }, m[5]));
    last = m.index + m[0].length;
  }
  if (last < text.length) spans.push(h(Text, { key: k++ }, text.slice(last)));
  return spans.length ? spans : [h(Text, { key: 0 }, text || " ")];
}

export function renderMarkdown(text, C) {
  const out = [];
  const lines = String(text).split("\n");
  let i = 0, key = 0;
  while (i < lines.length) {
    const line = lines[i];

    // fenced code block ``` … ```
    const fence = line.match(/^\s*```(\w*)/);
    if (fence) {
      const lang = fence[1];
      const code = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) { code.push(lines[i]); i++; }
      i++; // skip closing fence
      out.push(h(Box, {
        key: key++, flexDirection: "column", borderStyle: "round",
        borderColor: C.faint, paddingX: 1, marginY: 0,
      },
        lang ? h(Text, { color: C.dim, italic: true }, lang) : null,
        ...code.map((cl, j) => h(Text, { key: j, color: C.cyan }, cl.length ? cl : " "))));
      continue;
    }

    // header  #, ##, …
    const hd = line.match(/^\s*(#{1,6})\s+(.*)/);
    if (hd) {
      out.push(h(Text, { key: key++, bold: true, color: hd[1].length <= 2 ? C.purple : C.violet },
        ...inline(hd[2], C)));
      i++; continue;
    }

    // blockquote  >
    if (/^\s*>\s?/.test(line)) {
      out.push(h(Box, { key: key++ },
        h(Text, { color: C.faint }, "▏ "),
        h(Text, { color: C.dim }, ...inline(line.replace(/^\s*>\s?/, ""), C))));
      i++; continue;
    }

    // list item  -, *, +, or 1.
    const li = line.match(/^(\s*)([-*+]|\d+[.)])\s+(.*)/);
    if (li) {
      const bullet = /\d/.test(li[2]) ? li[2] : "•";
      out.push(h(Box, { key: key++ },
        li[1] ? h(Text, {}, li[1]) : null,
        h(Text, { color: C.amber }, `${bullet} `),
        h(Text, { color: C.text }, ...inline(li[3], C))));
      i++; continue;
    }

    // blank line → a little breathing room
    if (line.trim() === "") { out.push(h(Text, { key: key++ }, " ")); i++; continue; }

    // paragraph
    out.push(h(Text, { key: key++, color: C.text }, ...inline(line, C)));
    i++;
  }
  return out;
}
