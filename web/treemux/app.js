// Grove Terminal Viewer — Phase 4: DOM rendering
//
// Binary protocol (from compositor.zig ext_get_dirty_payload):
//   Header (12 bytes):
//     [0..4]   magic   "GRV1"
//     [4..6]   width   u16 LE
//     [6..8]   height  u16 LE
//     [8..12]  count   u32 LE  (number of cell records)
//
//   Cell record (16 bytes each):
//     [0..2]   x          u16 LE
//     [2..4]   y          u16 LE
//     [4..8]   codepoint  u32 LE
//     [8]      fg.r       u8
//     [9]      fg.g       u8
//     [10]     fg.b       u8
//     [11]     bg.r       u8
//     [12]     bg.g       u8
//     [13]     bg.b       u8
//     [14]     flags      u8 (bold:0, faint:1, italic:2, underline:3, inverse:4, strikethrough:5, wide:6, wide_spacer:7)
//     [15]     colorFlags u8 (fg_type:0-1, bg_type:2-3)

const HEADER_SIZE = 12;
const CELL_SIZE = 16;
const MAGIC = [0x47, 0x52, 0x56, 0x31]; // "GRV1"

const ColorType = {
    DEFAULT: 0,
    STANDARD: 1,
    PALETTE256: 2,
    RGB: 3,
};

// Standard ANSI 16-color palette (normal + bright)
const ANSI_COLORS = [
    '#282c34', '#e06c75', '#98c379', '#e5c07b', '#61afef', '#c678dd', '#56b6c2', '#abb2bf',
    '#5c6370', '#e06c75', '#98c379', '#e5c07b', '#61afef', '#c678dd', '#56b6c2', '#ffffff',
];

// 256-color palette: 0-15 standard, 16-231 color cube, 232-255 grayscale
const PALETTE_256 = (function () {
    const p = new Array(256);
    for (let i = 0; i < 16; i++) p[i] = ANSI_COLORS[i];
    for (let i = 0; i < 216; i++) {
        const r = Math.floor(i / 36), g = Math.floor((i % 36) / 6), b = i % 6;
        const toHex = function (v) { return v === 0 ? 0 : 55 + v * 40; };
        p[16 + i] = 'rgb(' + toHex(r) + ',' + toHex(g) + ',' + toHex(b) + ')';
    }
    for (let i = 0; i < 24; i++) {
        const v = 8 + i * 10;
        p[232 + i] = 'rgb(' + v + ',' + v + ',' + v + ')';
    }
    return p;
})();

const DEFAULT_FG = '#abb2bf';
const DEFAULT_BG = '#0d1117';

// --- Base64 decoding ---

function base64ToArrayBuffer(base64) {
    const bin = atob(base64);
    const buf = new ArrayBuffer(bin.length);
    const view = new Uint8Array(buf);
    for (let i = 0; i < bin.length; i++) {
        view[i] = bin.charCodeAt(i);
    }
    return buf;
}

// --- Binary decoder ---

function decodeHeader(view) {
    if (
        view.getUint8(0) !== MAGIC[0] ||
        view.getUint8(1) !== MAGIC[1] ||
        view.getUint8(2) !== MAGIC[2] ||
        view.getUint8(3) !== MAGIC[3]
    ) {
        return null;
    }
    return {
        width: view.getUint16(4, true),
        height: view.getUint16(6, true),
        count: view.getUint32(8, true),
    };
}

function unpackFlags(f) {
    return {
        bold: (f & 1) !== 0,
        faint: (f & 2) !== 0,
        italic: (f & 4) !== 0,
        underline: (f & 8) !== 0,
        inverse: (f & 16) !== 0,
        strikethrough: (f & 32) !== 0,
        wide: (f & 64) !== 0,
        widespacer: (f & 128) !== 0,
    };
}

function unpackColorFlags(f) {
    return {
        fgType: f & 0x3,
        bgType: (f >> 2) & 0x3,
    };
}

function decodeCell(view, offset) {
    const flags = unpackFlags(view.getUint8(offset + 14));
    const colorFlags = unpackColorFlags(view.getUint8(offset + 15));
    return {
        x: view.getUint16(offset, true),
        y: view.getUint16(offset + 2, true),
        codepoint: view.getUint32(offset + 4, true),
        fg: {
            type: colorFlags.fgType,
            r: view.getUint8(offset + 8),
            g: view.getUint8(offset + 9),
            b: view.getUint8(offset + 10),
        },
        bg: {
            type: colorFlags.bgType,
            r: view.getUint8(offset + 11),
            g: view.getUint8(offset + 12),
            b: view.getUint8(offset + 13),
        },
        flags: flags,
    };
}

function decodeFrame(base64Data) {
    const buf = base64ToArrayBuffer(base64Data);
    if (buf.byteLength < HEADER_SIZE) {
        console.warn('[grove] payload too small:', buf.byteLength, 'bytes');
        return null;
    }

    const view = new DataView(buf);
    const header = decodeHeader(view);
    if (!header) {
        console.warn('[grove] invalid magic bytes');
        return null;
    }

    const expectedSize = HEADER_SIZE + header.count * CELL_SIZE;
    if (buf.byteLength < expectedSize) {
        console.warn('[grove] truncated payload: expected', expectedSize, 'got', buf.byteLength);
        return null;
    }

    const cells = new Array(header.count);
    for (let i = 0; i < header.count; i++) {
        cells[i] = decodeCell(view, HEADER_SIZE + i * CELL_SIZE);
    }

    return { width: header.width, height: header.height, cells: cells };
}

// --- Color resolution ---

function resolveColor(color, fallback) {
    switch (color.type) {
        case ColorType.RGB:
            return 'rgb(' + color.r + ',' + color.g + ',' + color.b + ')';
        case ColorType.PALETTE256:
            return PALETTE_256[color.r] || fallback;
        case ColorType.STANDARD:
            return ANSI_COLORS[color.r] || fallback;
        default:
            return fallback;
    }
}

// --- DOM Grid ---

var gridWidth = 0;
var gridHeight = 0;
var gridCells = []; // flat array of span elements, indexed by y * width + x

function buildGrid(w, h) {
    var term = document.getElementById('terminal');
    var splash = document.getElementById('splash');

    splash.style.display = 'none';
    term.style.display = 'grid';
    term.style.gridTemplateColumns = 'repeat(' + w + ', 1ch)';
    term.style.gridTemplateRows = 'repeat(' + h + ', 1.2em)';
    term.innerHTML = '';

    gridCells = new Array(w * h);
    for (var i = 0; i < w * h; i++) {
        var span = document.createElement('span');
        span.textContent = ' ';
        span.style.color = DEFAULT_FG;
        span.style.backgroundColor = DEFAULT_BG;
        term.appendChild(span);
        gridCells[i] = span;
    }

    gridWidth = w;
    gridHeight = h;
}

function applyCell(cell) {
    if (cell.flags.widespacer) return; // skip right half of wide chars
    if (cell.x >= gridWidth || cell.y >= gridHeight) return;

    var idx = cell.y * gridWidth + cell.x;
    var span = gridCells[idx];
    if (!span) return;

    // Character
    var ch = cell.codepoint === 0 ? ' ' : String.fromCodePoint(cell.codepoint);
    span.textContent = ch;

    // Colors — handle inverse
    var fg = resolveColor(cell.fg, DEFAULT_FG);
    var bg = resolveColor(cell.bg, DEFAULT_BG);
    if (cell.flags.inverse) {
        var tmp = fg;
        fg = bg;
        bg = tmp;
    }
    span.style.color = fg;
    span.style.backgroundColor = bg;

    // Opacity for faint
    span.style.opacity = cell.flags.faint ? '0.5' : '';

    // Text decorations
    var deco = [];
    if (cell.flags.underline) deco.push('underline');
    if (cell.flags.strikethrough) deco.push('line-through');
    span.style.textDecoration = deco.length > 0 ? deco.join(' ') : '';

    // Font style
    span.style.fontWeight = cell.flags.bold ? 'bold' : '';
    span.style.fontStyle = cell.flags.italic ? 'italic' : '';

    // Wide characters: span 2 columns
    if (cell.flags.wide) {
        span.style.gridColumn = 'span 2';
        // Hide the spacer cell to the right
        var spacerIdx = idx + 1;
        if (spacerIdx < gridCells.length && gridCells[spacerIdx]) {
            gridCells[spacerIdx].style.display = 'none';
        }
    } else {
        span.style.gridColumn = '';
        span.style.display = '';
    }
}

function renderFrame(frame) {
    // Rebuild grid if dimensions changed or first frame
    if (frame.width !== gridWidth || frame.height !== gridHeight) {
        buildGrid(frame.width, frame.height);
    }

    for (var i = 0; i < frame.cells.length; i++) {
        applyCell(frame.cells[i]);
    }
}

// --- SSE connection ---

function connect() {
    var statusDot = document.getElementById('status-dot');
    var statusText = document.getElementById('connection-status');
    var frameInfo = document.getElementById('frame-info');
    var es = new EventSource('/api/treemux/stream');

    es.addEventListener('frame', function (e) {
        var frame = decodeFrame(e.data);
        if (!frame) return;

        renderFrame(frame);

        frameInfo.textContent =
            frame.width + '×' + frame.height +
            ' · ' + frame.cells.length + ' cells';
    });

    es.addEventListener('layout', function (e) {
        console.log('[grove] layout event:', e.data);
    });

    es.onopen = function () {
        statusDot.className = 'dot connected';
        statusText.textContent = 'Connected';
    };

    es.onerror = function () {
        statusDot.className = 'dot';
        statusText.textContent = 'Disconnected';
        frameInfo.textContent = '';
    };
}

connect();
