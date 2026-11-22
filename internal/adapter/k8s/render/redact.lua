-- AFS log redaction filter for the per-team Fluent Bit aggregator (RC-18, RC-US-16.1, RC-US-16.2).
-- It is the Lua twin of internal/usecase/redact (Go): the same patterns, the same token
-- [redacted:<first 8 hex of HMAC-SHA-256(team key, value)>], the same order. A parity test runs both
-- over one corpus. Change both together.
--
-- Inputs (mounted from Secrets only this pod reads):
--   /etc/afs/redact/key     the team's redaction key (raw bytes)
--   /etc/afs/redact/values  one base64 value per line: the team's bound secret values
-- Records are modified in place: every string value, at any depth, is redacted. Keys are left alone.

local bit = require("bit")
local band, bor, bxor, bnot, rshift, lshift, ror = bit.band, bit.bor, bit.bxor, bit.bnot, bit.rshift, bit.lshift, bit.ror

local KEY_PATH = os.getenv("AFS_REDACT_KEY_FILE") or "/etc/afs/redact/key"
local VALUES_PATH = os.getenv("AFS_REDACT_VALUES_FILE") or "/etc/afs/redact/values"
local RELOAD_SECONDS = 60
local MIN_KEY = 16
local MIN_SECRET = 6

-- SHA-256 ------------------------------------------------------------------------------------------

local K = {
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

local function u32bytes(n)
  return string.char(band(rshift(n, 24), 255), band(rshift(n, 16), 255), band(rshift(n, 8), 255), band(n, 255))
end

local function sha256(msg)
  local h0, h1, h2, h3, h4, h5, h6, h7 =
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19
  local len = #msg
  local bits = len * 8
  local hi = math.floor(bits / 4294967296)
  local lo = bits % 4294967296
  msg = msg .. "\128" .. string.rep("\0", (55 - len) % 64) .. u32bytes(hi) .. u32bytes(lo)
  local w = {}
  for chunk = 1, #msg, 64 do
    for i = 0, 15 do
      local a, b, c, d = msg:byte(chunk + i * 4, chunk + i * 4 + 3)
      w[i] = bor(lshift(a, 24), lshift(b, 16), lshift(c, 8), d)
    end
    for i = 16, 63 do
      local s0 = bxor(ror(w[i - 15], 7), ror(w[i - 15], 18), rshift(w[i - 15], 3))
      local s1 = bxor(ror(w[i - 2], 17), ror(w[i - 2], 19), rshift(w[i - 2], 10))
      w[i] = band(w[i - 16] + s0 + w[i - 7] + s1, 0xffffffff)
    end
    local a, b, c, d, e, f, g, h = h0, h1, h2, h3, h4, h5, h6, h7
    for i = 0, 63 do
      local S1 = bxor(ror(e, 6), ror(e, 11), ror(e, 25))
      local ch = bxor(band(e, f), band(bnot(e), g))
      local t1 = band(h + S1 + ch + K[i + 1] + w[i], 0xffffffff)
      local S0 = bxor(ror(a, 2), ror(a, 13), ror(a, 22))
      local maj = bxor(band(a, b), band(a, c), band(b, c))
      local t2 = band(S0 + maj, 0xffffffff)
      h, g, f, e, d, c, b, a = g, f, e, band(d + t1, 0xffffffff), c, b, a, band(t1 + t2, 0xffffffff)
    end
    h0, h1, h2, h3 = band(h0 + a, 0xffffffff), band(h1 + b, 0xffffffff), band(h2 + c, 0xffffffff), band(h3 + d, 0xffffffff)
    h4, h5, h6, h7 = band(h4 + e, 0xffffffff), band(h5 + f, 0xffffffff), band(h6 + g, 0xffffffff), band(h7 + h, 0xffffffff)
  end
  return u32bytes(h0) .. u32bytes(h1) .. u32bytes(h2) .. u32bytes(h3) .. u32bytes(h4) .. u32bytes(h5) .. u32bytes(h6) .. u32bytes(h7)
end

local function hmac_sha256(key, msg)
  if #key > 64 then key = sha256(key) end
  key = key .. string.rep("\0", 64 - #key)
  local ipad, opad = {}, {}
  for i = 1, 64 do
    local b = key:byte(i)
    ipad[i] = string.char(bxor(b, 0x36))
    opad[i] = string.char(bxor(b, 0x5c))
  end
  return sha256(table.concat(opad) .. sha256(table.concat(ipad) .. msg))
end

local function hex8(raw)
  return (raw:sub(1, 4):gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

-- Tokens and literals ---------------------------------------------------------------------------------

local function token(key, value)
  return "[redacted:" .. hex8(hmac_sha256(key, value)) .. "]"
end

local function is_token(s)
  return s:match("^%[redacted:[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]%]$") ~= nil
end

local function json_escape(s)
  return (s:gsub('[%c"\\<>&]', function(c)
    if c == '"' then return '\\"' end
    if c == "\\" then return "\\\\" end
    if c == "\n" then return "\\n" end
    if c == "\r" then return "\\r" end
    if c == "\t" then return "\\t" end
    return string.format("\\u%04x", c:byte())
  end))
end

local function url_escape(s)
  return (s:gsub("[^%w%-_%.~]", function(c)
    if c == " " then return "+" end
    return string.format("%%%02X", c:byte())
  end))
end

local B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
local B64IDX = {}
for i = 1, #B64 do B64IDX[B64:byte(i)] = i - 1 end

local function b64decode(s)
  local out, acc, n = {}, 0, 0
  for i = 1, #s do
    local v = B64IDX[s:byte(i)]
    if v then
      acc = acc * 64 + v
      n = n + 1
      if n == 4 then
        out[#out + 1] = string.char(math.floor(acc / 65536) % 256, math.floor(acc / 256) % 256, acc % 256)
        acc, n = 0, 0
      end
    end
  end
  if n == 2 then
    out[#out + 1] = string.char(math.floor(acc / 16) % 256)
  elseif n == 3 then
    out[#out + 1] = string.char(math.floor(acc / 1024) % 256, math.floor(acc / 4) % 256)
  end
  return table.concat(out)
end

-- literals: bound secret values in their plain, JSON-escaped and URL-escaped spellings, longest first.
local function build_literals(key, values)
  local lits, seen = {}, {}
  for _, raw in ipairs(values) do
    if #raw >= MIN_SECRET then
      local tok = token(key, raw)
      for _, sp in ipairs({ raw, json_escape(raw), url_escape(raw) }) do
        if not seen[sp] then
          seen[sp] = true
          lits[#lits + 1] = { text = sp, token = tok }
        end
      end
    end
  end
  table.sort(lits, function(a, b)
    if #a.text ~= #b.text then return #a.text > #b.text end
    return a.text < b.text
  end)
  return lits
end

local function replace_plain(s, text, repl)
  local out, pos = {}, 1
  while true do
    local i, j = s:find(text, pos, true)
    if not i then break end
    out[#out + 1] = s:sub(pos, i - 1)
    out[#out + 1] = repl
    pos = j + 1
  end
  if pos == 1 then return s end
  out[#out + 1] = s:sub(pos)
  return table.concat(out)
end

-- Patterns (twins of internal/usecase/redact) ---------------------------------------------------------

local function is_word(c) return c ~= nil and c ~= "" and c:match("[%w_]") ~= nil end

local function has_digit(s) return s:find("%d") ~= nil end

-- replace_spans applies f to each (start, end) span, left to right; f returns the replacement or nil.
local function rebuild(s, spans)
  if #spans == 0 then return s end
  local out, pos = {}, 1
  for _, sp in ipairs(spans) do
    out[#out + 1] = s:sub(pos, sp[1] - 1)
    out[#out + 1] = sp[3]
    pos = sp[2] + 1
  end
  out[#out + 1] = s:sub(pos)
  return table.concat(out)
end

local function private_keys(s, key)
  local spans, pos = {}, 1
  while true do
    local i, j = s:find("%-%-%-%-%-BEGIN [A-Z ]*PRIVATE KEY%-%-%-%-%-", pos)
    if not i then break end
    local _, e = s:find("%-%-%-%-%-END [A-Z ]*PRIVATE KEY%-%-%-%-%-", j + 1)
    e = e or #s
    spans[#spans + 1] = { i, e, token(key, s:sub(i, e)) }
    pos = e + 1
  end
  return rebuild(s, spans)
end

local function jwts(s, key)
  local spans, pos = {}, 1
  while true do
    local i = s:find("eyJ", pos, true)
    if not i then break end
    local advance = i + 1
    if i == 1 or not is_word(s:sub(i - 1, i - 1)) then
      -- eyJ, then 5 or more characters, ".", 5 or more, ".", any number
      local seg1, seg2, seg3 = s:match("^eyJ([%w_%-]*)%.([%w_%-]*)%.([%w_%-]*)", i)
      if seg1 and #seg1 >= 5 and #seg2 >= 5 then
        local e = i + 2 + #seg1 + 1 + #seg2 + 1 + #seg3
        spans[#spans + 1] = { i, e, token(key, s:sub(i, e)) }
        advance = e + 1
      end
    end
    pos = advance
  end
  return rebuild(s, spans)
end

local AWS_PREFIXES = { ["AKIA"] = true, ["ASIA"] = true, ["AGPA"] = true, ["AIDA"] = true, ["AROA"] = true, ["ANPA"] = true, ["ANVA"] = true, ["AIPA"] = true }

local function aws_key_ids(s, key)
  local spans = {}
  local i = 1
  while i <= #s - 19 do
    if AWS_PREFIXES[s:sub(i, i + 3)] and (i == 1 or not is_word(s:sub(i - 1, i - 1))) then
      local body = s:sub(i + 4, i + 19)
      if body:match("^[A-Z0-9]+$") and not is_word(s:sub(i + 20, i + 20)) then
        spans[#spans + 1] = { i, i + 19, token(key, s:sub(i, i + 19)) }
        i = i + 20
      else
        i = i + 1
      end
    else
      i = i + 1
    end
  end
  return rebuild(s, spans)
end

local function url_passwords(s, key)
  local spans, pos = {}, 1
  while pos <= #s do
    local i, j, pw = s:find("%a[%w%+%.%-]*://[^%s/:@]*:([^%s/@]+)@", pos)
    if not i then break end
    if i > 1 and is_word(s:sub(i - 1, i - 1)) then
      pos = i + 1
    else
      local ps = j - #pw -- position of "@" is j; the password ends at j-1
      spans[#spans + 1] = { j - #pw, j - 1, token(key, pw) }
      pos = j + 1
    end
  end
  return rebuild(s, spans)
end

local KEYWORDS = { "password", "passwd", "pwd", "secret", "token", "credential", "credentials", "authorization" }

local function key_is_credential(run)
  run = run:lower()
  for _, k in ipairs(KEYWORDS) do
    if #run >= #k and run:sub(-#k) == k then return true end
  end
  for _, k in ipairs({ "apikey", "api_key", "api-key", "accesskey", "access_key", "access-key", "privatekey", "private_key", "private-key" }) do
    if #run >= #k and run:sub(-#k) == k then return true end
  end
  return false
end

local STOP = "[%s,;&\"'}%]]"

-- value_at parses a credential value at position p, returning its first and last index or nil.
local function value_at(s, p)
  local c = s:sub(p, p)
  if c == '"' or c == "'" then
    local close = s:find(c, p + 1, true)
    local nl = s:find("\n", p + 1, true)
    if close and (not nl or nl > close) then
      if close == p + 1 then return nil end -- empty
      return p + 1, close - 1
    end
    return nil
  end
  local function run_from(q)
    local e = q
    while e <= #s and not s:sub(e, e):match(STOP) do e = e + 1 end
    if e > q then return q, e - 1 end
    return nil
  end
  local function tok_or_run(q)
    local a, b = s:find("^%[redacted:[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]%]", q)
    if a then return a, b end
    return run_from(q)
  end
  local ls = s:sub(p):lower()
  for _, w in ipairs({ "bearer", "basic" }) do
    if ls:sub(1, #w) == w then
      local sp = s:match("^%s+", p + #w)
      if sp then
        local a, b = tok_or_run(p + #w + #sp)
        if a then return p, b end
      end
    end
  end
  return tok_or_run(p)
end

local function key_values(s, key)
  local spans, pos = {}, 1
  while pos <= #s do
    local sep = s:find("[=:]", pos)
    if not sep then break end
    -- walk back over spaces, one quote, then the run of key characters
    local i = sep - 1
    while i >= pos and s:sub(i, i):match("%s") do i = i - 1 end
    if i >= pos and s:sub(i, i):match("[\"']") then i = i - 1 end
    local e = i
    while i >= pos and s:sub(i, i):match("[%w_%.%-]") do i = i - 1 end
    local run = s:sub(i + 1, e)
    local advance = sep + 1
    if #run > 0 and key_is_credential(run) then
      local vp = sep + 1
      local sp = s:match("^%s*", vp)
      vp = vp + #sp
      local a, b = value_at(s, vp)
      if a then
        local v = s:sub(a, b)
        if not is_token(v) then
          spans[#spans + 1] = { a, b, token(key, v) }
        end
        advance = b + 1
        if s:sub(a - 1, a - 1) == '"' or s:sub(a - 1, a - 1) == "'" then advance = b + 2 end
      end
    end
    pos = advance
  end
  return rebuild(s, spans)
end

local function bearers(s, key)
  local ls = s:lower()
  local spans = {}
  local pos = 1
  while pos <= #s do
    local bi, bj = ls:find("bearer", pos, true)
    local si, sj = ls:find("basic", pos, true)
    local i, j
    if bi and (not si or bi <= si) then i, j = bi, bj else i, j = si, sj end
    if not i then break end
    local advance = i + 1
    if not (i > 1 and is_word(s:sub(i - 1, i - 1))) then
      local sp = s:match("^%s+", j + 1)
      if sp then
        local q = j + 1 + #sp
        local tok = s:match("^[%w%._~%+/=%-]+", q)
        if tok and #tok >= 8 and has_digit(tok) and not is_token(tok) then
          spans[#spans + 1] = { q, q + #tok - 1, token(key, tok) }
          advance = q + #tok
        end
      end
    end
    pos = advance
  end
  return rebuild(s, spans)
end

local function redact_string(s, key, lits)
  for _, l in ipairs(lits) do
    s = replace_plain(s, l.text, l.token)
  end
  s = private_keys(s, key)
  s = jwts(s, key)
  s = aws_key_ids(s, key)
  s = url_passwords(s, key)
  s = key_values(s, key)
  s = bearers(s, key)
  return s
end

-- Loading and the Fluent Bit entry point -------------------------------------------------------------

local state = { key = nil, lits = {}, loaded = 0 }

local function read_file(path)
  local f = io.open(path, "rb")
  if not f then return nil end
  local d = f:read("*a")
  f:close()
  return d
end

local function load_state()
  local key = read_file(KEY_PATH)
  if key and #key >= MIN_KEY then
    local values = {}
    local vf = read_file(VALUES_PATH)
    if vf then
      for line in vf:gmatch("[^\r\n]+") do
        local v = b64decode(line)
        if #v > 0 then values[#values + 1] = v end
      end
    end
    state.key, state.lits = key, build_literals(key, values)
  end
  state.loaded = os.time()
end

local function walk(v, key, lits)
  if type(v) == "string" then
    return redact_string(v, key, lits)
  elseif type(v) == "table" then
    for k, x in pairs(v) do
      local nx = walk(x, key, lits)
      if nx ~= x then v[k] = nx end
    end
  end
  return v
end

-- redact is the Fluent Bit filter callback. Without a key the aggregator must not deliver anything
-- unredacted, so every string is replaced (fail closed).
function redact(tag, timestamp, record)
  if os.time() - state.loaded >= RELOAD_SECONDS or state.loaded == 0 then load_state() end
  if not state.key then
    for k, v in pairs(record) do
      if type(v) == "string" then record[k] = "[redacted:nokey]" end
    end
    return 2, timestamp, record
  end
  walk(record, state.key, state.lits)
  return 2, timestamp, record
end

-- Exposed for the parity test (the test runs this file with luajit, outside Fluent Bit).
AFS_REDACT = { b64decode = b64decode, redact_string = redact_string, build_literals = build_literals, token = token, sha256 = sha256, hmac = hmac_sha256 }
