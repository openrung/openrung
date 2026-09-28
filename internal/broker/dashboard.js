// SPDX-License-Identifier: GPL-3.0-or-later
// Shared helpers for the admin telemetry pages (overview and relays):
// escaping and formatting, relay class markers, relative times, donut
// breakdowns, tap-to-copy, and the header's freshness label with optional
// auto-refresh. Loaded as a classic script ahead of each page's own code.
const $=id=>document.getElementById(id), esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const number=value=>new Intl.NumberFormat().format(value||0);
const dateTime=value=>value?new Intl.DateTimeFormat(undefined,{dateStyle:'short',timeStyle:'short'}).format(new Date(value)):'—';
function status(message,error=false){$('status').textContent=message;$('status').className='status'+(message?' show':'')+(error?' error':'')}

// Mutations always declare a JSON body (even a bodiless DELETE): the weight
// endpoints refuse anything else, which is what keeps a cross-site form post
// from reaching them alongside the session cookie's SameSite=Strict.
async function fetchJSON(path,{method='GET',body}={}){const headers={Accept:'application/json'};if(method!=='GET')headers['Content-Type']='application/json';const response=await fetch(path,{method,headers,body:body===undefined?undefined:JSON.stringify(body)});if(response.status===401){location.href='/admin/telemetry/login';throw new Error('signed out')}if(!response.ok)throw new Error((await response.json().catch(()=>({}))).error||`Server returned ${response.status}`);return response.json()}

// A relay's broker-attested node class is shown by colouring its name:
// bright green for foundation, orange for volunteer. The class is mapped to a
// fixed CSS class rather than interpolated. Each name also carries a visible
// FND/VOL marker plus an accessible label, and nodeTitle repeats the full
// class in the hover tooltip. Rankings of anything but relays carry no
// node_class and render unchanged.
function relayName(text,cls){const c=cls==='foundation'?'relay-foundation':cls==='volunteer'?'relay-volunteer':'';if(!c)return text;const marker=cls==='foundation'?'FND':'VOL';return `<span class="${c}">${text}</span><span class="relay-class-marker ${c}" aria-label="${esc(cls)} relay">${marker}</span>`}
function nodeTitle(c){return c?` · ${esc(c)}`:''}

// Relative times ("5m ago") with the absolute stamp on hover. tick() rewrites
// every rendered <time data-rel> so they stay current between loads.
function relTime(value){
  const seconds=Math.max(0,(Date.now()-new Date(value))/1000);
  if(seconds<45)return 'just now';
  if(seconds<3600)return Math.max(1,Math.floor(seconds/60))+'m ago';
  if(seconds<86400)return Math.floor(seconds/3600)+'h ago';
  if(seconds<7*86400)return Math.floor(seconds/86400)+'d ago';
  return new Intl.DateTimeFormat(undefined,{dateStyle:'medium'}).format(new Date(value));
}
function timeCell(value,label=''){return value?`<time data-rel="${esc(value)}" title="${esc((label?label+' · ':'')+dateTime(value))}">${esc(relTime(value))}</time>`:'—'}
function tick(){for(const el of document.querySelectorAll('time[data-rel]'))el.textContent=relTime(el.dataset.rel)}

// Donut breakdowns. Ranked panels take five categorical slices (validated for
// colour-vision deficiency on this surface, in ring order) plus a neutral grey
// for the folded remainder.
const PIE_COLORS=['#3987e5','#d95926','#199e70','#c98500','#d55181'],PIE_OTHER='#9aa89f';
const otherSlice=rest=>({name:`Other (${rest.length})`,count:rest.reduce((n,x)=>n+x.count,0),color:PIE_OTHER});
function rankedSlices(items){const top=items.slice(0,PIE_COLORS.length).map((x,i)=>({...x,color:PIE_COLORS[i]})),rest=items.slice(PIE_COLORS.length);return rest.length?[...top,otherSlice(rest)]:top}
// OS families keep one colour and one ring position whatever their rank, so
// the panel reads the same across windows. The ring order reuses the validated
// adjacent sequence; only a family's absence brings weaker pairs together, and
// the legend names every slice.
const OS_ORDER=['Windows','Linux','Android','iOS','macOS'];
function osSlices(items){const byName=new Map(items.map(x=>[x.name,x])),rest=items.filter(x=>!OS_ORDER.includes(x.name));const slices=OS_ORDER.flatMap((name,i)=>byName.has(name)?[{...byName.get(name),color:PIE_COLORS[i]}]:[]);return rest.length?[...slices,otherSlice(rest)]:slices}
// donut renders one breakdown. Options: slices (colour assignment), children
// (sub-rows listed under a slice's legend entry), limit (the server-side cap,
// noted when reached), note, empty (message with no data) and unit (centre).
function donut(items,{slices=rankedSlices,children,limit,note='',empty='No telemetry in this window',unit='total'}={}){
  if(!items?.length)return `<div class="empty">${esc(empty)}</div>`;
  const parts=slices(items),total=parts.reduce((n,x)=>n+x.count,0),r=54,c=2*Math.PI*r,gap=parts.length>1?2:0;let offset=0;
  const pct=n=>{const share=n/total*100;return share.toFixed(share<9.95?1:0)+'%'};
  const value=(x,cls='')=>`<button type="button" class="pct${cls}" data-alt="${number(x.count)}" aria-label="${esc(x.label||x.name)}: ${pct(x.count)}, ${number(x.count)}. Tap to swap all.">${pct(x.count)}</button>`;
  const arcs=parts.map(x=>{const len=x.count/total*c,arc=`<circle class="slice" cx="74" cy="74" r="${r}" fill="none" stroke="${x.color}" stroke-width="24" stroke-dasharray="${Math.max(0,len-gap)} ${c}" stroke-dashoffset="${-offset}" transform="rotate(-90 74 74)"><title>${esc(x.label||x.name)}</title></circle>`;offset+=len;return arc}).join('');
  const legend=parts.map(x=>`<i class="swatch" style="background:${x.color}"></i><span class="rank-name" title="${esc(x.name)}${nodeTitle(x.node_class)}">${esc(x.label||x.name)}${x.node_class?relayName('',x.node_class):''}</span>${value(x)}`+(children?.(x)||[]).map(y=>`<i></i><span class="rank-name sub" title="${esc(y.name)}">${esc(y.name)}</span>${value(y,' sub')}`).join('')).join('');
  const notes=[limit&&items.length>=limit?`Share among the top ${limit} shown`:'',note].filter(Boolean).map(n=>`<span class="pie-note">${esc(n)}</span>`).join('');
  return `<div class="pie"><svg viewBox="0 0 148 148" role="img" aria-label="Breakdown of ${number(total)}">${arcs}<text class="pie-total" x="74" y="76" text-anchor="middle">${number(total)}</text><text class="pie-sub" x="74" y="92" text-anchor="middle">${esc(unit)}</text></svg><div class="pie-legend">${legend}${notes}</div></div>`;
}
// keyedSlices colours entries by a fixed list of names (the rest fold into
// Other), so the same entity wears the same colour in every donut using it.
function keyedSlices(names){return items=>{const parts=items.filter(x=>names.includes(x.name)).map(x=>({...x,color:PIE_COLORS[names.indexOf(x.name)]})),rest=items.filter(x=>!names.includes(x.name));return rest.length?[...parts,otherSlice(rest)]:parts}}
// pair renders two titled donuts side by side (active now vs the window),
// colouring both by the last non-empty side's top entries so an entity
// matches across.
function pair(sides){
  if(sides.every(s=>!s.items?.length))return '<div class="empty">No telemetry in this window</div>';
  const key=sides.filter(s=>s.items?.length).at(-1),slices=keyedSlices(key.items.slice(0,PIE_COLORS.length).map(x=>x.name));
  return `<div class="pair">${sides.map(s=>`<div><div class="pie-title">${esc(s.title)}</div>${donut(s.items,{slices,...s})}</div>`).join('')}</div>`;
}

// Copy the full value behind a shortened ID. The clipboard API needs a secure
// context; the textarea fallback covers plain-HTTP previews.
async function copyText(text){try{await navigator.clipboard.writeText(text);return true}catch{const area=document.createElement('textarea');area.value=text;area.style.cssText='position:fixed;opacity:0';document.body.append(area);area.select();let ok=false;try{ok=document.execCommand('copy')}catch{}area.remove();return ok}}
function copyable(full,shown){return full?`<button type="button" class="copyable" data-copy="${esc(full)}" title="${esc(full)} — tap to copy">${esc(shown)}</button>`:'—'}

// One delegated listener serves every page: tapping a donut value or slice
// swaps the whole panel between shares and raw counts, and tapping a
// shortened ID copies its full value (click covers mouse, touch and keyboard).
document.addEventListener('click',async event=>{
  const flip=event.target.closest('.pie button.pct, .pie circle.slice');
  if(flip){for(const value of flip.closest('.panel').querySelectorAll('.pie button.pct'))[value.textContent,value.dataset.alt]=[value.dataset.alt,value.textContent];return}
  const copy=event.target.closest('[data-copy]');
  if(copy){const ok=await copyText(copy.dataset.copy);copy.dataset.flash=ok?'copied':'copy failed';copy.classList.add('flash');clearTimeout(copy.flashTimer);copy.flashTimer=setTimeout(()=>copy.classList.remove('flash'),1200)}
});

// setupRefresh wires the header's freshness label and the optional
// auto-refresh interval (a per-viewer convenience kept in localStorage).
// load({quiet:true}) is the automatic reload; busy() lets a page skip one,
// e.g. while an operator is typing a relay weight.
function setupRefresh({load,busy=()=>false}){
  const auto=$('autoRefresh');let timer=0;
  try{auto.value=localStorage.getItem('openrung.autoRefresh')||'0'}catch{}
  if(!auto.value)auto.value='0';
  const schedule=()=>{clearInterval(timer);const every=Number(auto.value);if(every)timer=setInterval(()=>{if(!document.hidden&&!busy())load({quiet:true})},every*1000)};
  auto.addEventListener('change',()=>{try{localStorage.setItem('openrung.autoRefresh',auto.value)}catch{}schedule()});
  schedule();setInterval(tick,15000);
}
function markUpdated(value){$('updated').innerHTML=`Updated ${timeCell(value)}`}
