// jsoup 风格的 pdfh/pdfa/pd/pdfl 近似实现（drpy 的 hiker 语法）
import * as cheerio from '../libs/cheerio_tv.mjs';
const load=(h)=>cheerio.load(h||'',{},false);
const NOEQ=/:eq|:lt|:gt|:first|:last|^body$|^#/;
function splitEx(p){ const [sel,...ex]=p.split('--'); return [sel,ex]; }
function find($,ctx,part,first){
  const [selFull,ex]=splitEx(part.trim());
  let sel=selFull, els;
  try{
    if(first && !NOEQ.test(sel)) sel=sel+':eq(0)';
    // cheerio 不认 :eq(0) 在复合选择器末尾之外的位置，这里把 :eq(n) 转 .eq
    const m=sel.match(/^(.*?):eq\((-?\d+)\)$/);
    if(m){ let all=m[1]?ctx.find(m[1]):ctx; if(!m[1]) all=ctx; const n=parseInt(m[2]); els=all.eq(n<0?all.length+n:n); }
    else els=ctx.find(sel.replace(/:eq\((-?\d+)\)/g,(a,n)=>`:nth-child(${parseInt(n)+1})`));
    for(const e of ex){ if(e) els.find(e).remove(); }
  }catch(e){ els=$([]); }
  // 片段模式解析没有 <body>/<html> 元素：规则里常见的 body&&xxx 把 body 当作当前上下文
  if(!els.length && /^(body|html)$/.test(selFull.trim())) els=ctx;
  return els;
}
function chain($,rootEl,rule,first){
  let ctx=rootEl; for(const p of rule.split('&&')){ ctx=find($,ctx,p,first); if(!ctx.length) return ctx; if(first&&ctx.length>1) ctx=ctx.first(); } return ctx;
}
export function pdfa(html,rule){ if(!html||!rule) return []; if(typeof html!=='string') html=String(html);
  const $=load(html); const root=$.root(); let ctx;
  for(const alt of rule.split('||')){ ctx=chain($,root,alt.replace(/&&$/,''),false); if(ctx.length) break; }
  return ctx.map((i,e)=>$.html(e)).get(); }
export function pdfh(html,rule){ if(!html||!rule) return ''; if(typeof html!=='string') html=String(html);
  const $=load(html); const root=$.root();
  for(const alt of rule.split('||')){
    let ps=alt.split('&&'), option=null;
    if(ps.length>1){ option=ps.pop(); }
    let el=chain($,root,ps.join('&&'),true); if(!el.length) continue; el=el.first();
    let v;
    if(!option) v=$.html(el);
    else if(option==='Text') v=el.text();
    else if(option==='Html') v=el.html();
    else v=el.attr(option)||'';
    if(v) return typeof v==='string'?v.trim():v;
  }
  return '';
}
export function joinUrl(a,b){ try{ return new URL(b,a||undefined).href }catch(e){ return b } }
const URLATTR=/(url|src|href|-original|-src|-play|-url|style)$/;
export function pd(html,rule,base){
  const opt=rule&&rule.includes('&&')?rule.split('||').map(x=>x.split('&&').pop()).find(o=>URLATTR.test(o)):null;
  let v=pdfh(html,rule); if(!v) return ''; if(!opt) return v;
  if(/^style$|style$/.test(opt)){ const m=v.match(/url\(['"]?(.*?)['"]?\)/); if(m) v=m[1]; }
  if(/^(ftp|magnet|thunder|ws|javascript):/i.test(v)) return v;
  return base?joinUrl(base,v):v;
}
export function pdfl(html,rule,list_text,list_url,url_key){
  const items=pdfa(html,rule); return items.map(it=>{ const t=pdfh(it,list_text); let u=pdfh(it,list_url); return t+'$'+u }); }
Object.assign(globalThis,{pdfh,pdfa,pd,pdfl,joinUrl,base64Encode:s=>Buffer.from(String(s)).toString('base64'),base64Decode:s=>Buffer.from(String(s),'base64').toString()});
