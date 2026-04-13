// Grove Terminal Viewer — Phase 2: SSE connection + binary decoder
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

// Color type enum values (matches ansi.ColorType in Zig)
const ColorType = {
    DEFAULT: 0,
    STANDARD: 1,
    PALETTE256: 2,
    RGB: 3,
};

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
    // Validate magic
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

// --- SSE connection ---

function connect() {
    const statusEl = document.getElementById('connection-status');
    const frameInfo = document.getElementById('frame-info');
    const es = new EventSource('/api/terminal/stream');

    es.addEventListener('frame', function (e) {
        const frame = decodeFrame(e.data);
        if (!frame) return;

        statusEl.textContent = 'Connected';
        statusEl.style.color = '#4ecca3';
        frameInfo.textContent =
            frame.width + 'x' + frame.height +
            ' — ' + frame.cells.length + ' cells updated';

        // Log first few cells for verification
        if (frame.cells.length > 0) {
            const sample = frame.cells.slice(0, 5);
            console.log(
                '[grove] frame: %dx%d, %d cells. Sample:',
                frame.width, frame.height, frame.cells.length,
                sample.map(function (c) {
                    return {
                        pos: c.x + ',' + c.y,
                        char: String.fromCodePoint(c.codepoint),
                        cp: c.codepoint,
                        fg: c.fg,
                        bg: c.bg,
                        flags: c.flags,
                    };
                })
            );
        }
    });

    es.addEventListener('layout', function (e) {
        console.log('[grove] layout event:', e.data);
    });

    es.onopen = function () {
        statusEl.textContent = 'Connected';
        statusEl.style.color = '#4ecca3';
    };

    es.onerror = function () {
        statusEl.textContent = 'Disconnected';
        statusEl.style.color = '#e74c3c';
        frameInfo.textContent = '';
    };
}

connect();
