import{H as e,N as t,O as n,St as r,V as i,X as a,_ as o,a as s,bt as c,d as l,g as u,j as d,l as f,n as p,p as m,u as h}from"./client-DZA8RQqG.js";import{h as g,r as _,u as v}from"./index-DxaJzfTp.js";import{t as y}from"./Button-DavtFOFI.js";import{t as b}from"./Card-DiDQazpl.js";import{t as x}from"./Badge-CJAFzYrZ.js";import{t as S}from"./CustomHome-DHPI1d3v.js";var ee={class:`hc`},te={class:`hc-head`},C={class:`hc-sub`},w={class:`hc-actions`},T={class:`hc-grid`},E={class:`hc-edit`},D={class:`hc-pane`},ne={class:`hc-pane-head`},re={class:`hc-pane`},ie={class:`hc-pane-head`},O={class:`hc-pane hc-pane-vars`},k={class:`hc-vars`},A=[`onClick`],j={class:`hc-pane hc-pane-ai`},M={class:`hc-pane-head`},N={class:`hc-preview`},P={class:`hc-frame`},F={key:1,class:`hc-starter`},I=`帮我写一个静态网页首页，用于代理面板的公开首页（访客打开域名时看到的第一个页面）。

【输出格式】
只输出两个代码块，不要任何解释文字：
1. 第一个代码块标注 html
2. 第二个代码块标注 css

【页面要求】
- 单文件页面，HTML 和 CSS 分开给我，不要合并成style 标签写在 head 里
- 结构用语义化标签（header / section / footer / h1 / h2 / p / a / button）
- 不要用任何 JavaScript，不要用 <script>、<iframe>、<form>、<object>、<embed>，这些会被自动剥掉
- 不要引入外部 JS 框架或 CSS 框架（Tailwind、Bootstrap 等），页面只允许 HTML + 纯 CSS
- 图片只能用直链 URL，优先用 CSS渐变和纯色块代替图片

【风格要求】
- 浅色主题，整体克制、有质感，不要花哨渐变和重阴影
- 正文用系统无衬线字体：font-family: 'Inter', system-ui, -apple-system, 'Segoe UI', sans-serif
- 大标题想要编辑感可以用衬线体：font-family: 'Cormorant Garamond', Georgia, serif
- 面板主题色是近黑色 hsl(240 5.9% 10%)，按钮和强调文字用这个色系
- 留白要充足，字号层级分明，桌面端为主，移动端不能横向滚动
- 不要用 emoji 做图标

【可用变量】
在 HTML 里直接写下面的占位符，页面会自动替换成真实内容：
{{site_title}}站点名称
{{site_subtitle}}   站点副标题
{{year}}            当前年份

【CSS 限制】
- CSS 会自动加作用域前缀，只作用于首页内容区
- 不要写 body {}、html {}、:root {}，这些会被自动改写
- 不要用 position: fixed，不要用负数 z-index，会被降级
- 需要整页背景色的话，写在 .ch-custom-home 这个 class 上

【内容建议】
包含：顶部导航、主视觉（标题+一句话说明+行动按钮）、2-3 个卖点/优势介绍、数据或节点列表、页脚
文案用中文，不要 lorem ipsum 这类占位文字，写真实可信的说明。`,L=`<div class="hero">
  <h1>{{site_title}}</h1>
  <p>用 HTML 和 CSS 自己搭这个页面</p>
  <a class="btn" href="/login">立即登录</a>
</div>`,R=`.hero {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 16px;
  text-align: center;
}
.btn {
  padding: 10px 24px;
  border-radius: 8px;
  background: #111;
  color: #fff;
  text-decoration: none;
}`,z=p({__name:`HomeCustom`,setup(p){let z=v(),B=a(``),V=a(``),H=a(!1),U=a(!1),W=a(1048576),G=a(262144),K=[`{{site_title}}`,`{{site_subtitle}}`,`{{year}}`],q=async()=>{let e=()=>{let e=document.querySelector(`.hc-ai-preview`);if(!e)return!1;e.scrollIntoView({block:`center`});let t=document.createRange();t.selectNodeContents(e);let n=window.getSelection();return n.removeAllRanges(),n.addRange(t),!0};try{if(navigator.clipboard&&window.isSecureContext){await navigator.clipboard.writeText(I),z.success(`提示词已复制，粘贴给 AI 后把返回的代码填进下面两个框`);return}}catch{}try{let e=document.createElement(`textarea`);e.value=I,e.style.position=`fixed`,e.style.top=`-9999px`,e.setAttribute(`readonly`,``),document.body.appendChild(e),e.select();let t=document.execCommand(`copy`);if(e.remove(),t){z.success(`提示词已复制，粘贴给 AI 后把返回的代码填进下面两个框`);return}}catch{}e()?z.error(`浏览器阻止了自动复制，文本已选中，按 Ctrl+C 手动复制`):z.error(`复制失败，请手动选中提示词文本复制`)},J=f(()=>new Blob([B.value]).size),Y=f(()=>new Blob([V.value]).size),X=e=>e<1024?`${e} B`:e<1024*1024?`${(e/1024).toFixed(1)} KB`:`${(e/1024/1024).toFixed(2)} MB`,Z=f(()=>J.value>W.value),Q=f(()=>Y.value>G.value),$=f(()=>B.value.trim()!==``),ae=async()=>{H.value=!0;try{let e=await _.getHomeCustom();e&&(B.value=e.html||``,V.value=e.css||``,W.value=e.max_html||W.value,G.value=e.max_css||G.value)}catch(e){z.error(e?.message||`加载失败`)}finally{H.value=!1}},oe=async()=>{if(Z.value)return z.error(`HTML 超出上限（最多 ${X(W.value)}）`);if(Q.value)return z.error(`CSS 超出上限（最多 ${X(G.value)}）`);U.value=!0;try{await _.saveHomeCustom({html:B.value,css:V.value}),z.success(`已保存，公开首页已生效`)}catch(e){z.error(e?.message||`保存失败`)}finally{U.value=!1}},se=async()=>{if(confirm(`清空自定义内容并恢复默认首页？此操作不可撤销。`)){U.value=!0;try{await _.resetHomeCustom(),B.value=``,V.value=``,z.success(`已恢复默认首页`)}catch(e){z.error(e?.message||`恢复失败`)}finally{U.value=!1}}},ce=e=>{let t=document.getElementById(`home-html-editor`);if(!t)return;let n=t.selectionStart??B.value.length,r=t.selectionEnd??B.value.length;B.value=B.value.slice(0,n)+e+B.value.slice(r),requestAnimationFrame(()=>{t.focus(),t.setSelectionRange(n+e.length,n+e.length)})};return n(ae),(n,a)=>(d(),m(`div`,ee,[h(`div`,te,[h(`div`,null,[a[4]||=h(`h1`,{class:`hc-title`},`主页`,-1),h(`p`,C,[a[3]||=u(` 用 HTML + CSS 自定义公开首页（访客打开域名时看到的页面）。 `,-1),o(x,{variant:$.value?`success`:`info`},{default:i(()=>[u(r($.value?`自定义已启用`:`当前使用默认首页`),1)]),_:1},8,[`variant`])])]),h(`div`,w,[o(y,{variant:`secondary`,size:`sm`,loading:U.value,onClick:se},{default:i(()=>[...a[5]||=[u(`恢复默认`,-1)]]),_:1},8,[`loading`]),o(y,{size:`sm`,loading:U.value,onClick:oe},{default:i(()=>[...a[6]||=[u(`保存并发布`,-1)]]),_:1},8,[`loading`])])]),h(`div`,T,[h(`div`,E,[o(b,{padding:!1},{default:i(()=>[h(`div`,D,[h(`div`,ne,[a[7]||=h(`span`,{class:`hc-pane-title`},`HTML`,-1),h(`span`,{class:c([`hc-size`,{"is-over":Z.value}])},r(X(J.value))+` / `+r(X(W.value)),3)]),e(h(`textarea`,{id:`home-html-editor`,"onUpdate:modelValue":a[0]||=e=>B.value=e,class:`hc-code`,rows:`14`,spellcheck:`false`,placeholder:`在这里写 HTML…`},null,512),[[g,B.value]])]),h(`div`,re,[h(`div`,ie,[a[8]||=h(`span`,{class:`hc-pane-title`},`CSS`,-1),h(`span`,{class:c([`hc-size`,{"is-over":Q.value}])},r(X(Y.value))+` / `+r(X(G.value)),3)]),e(h(`textarea`,{"onUpdate:modelValue":a[1]||=e=>V.value=e,class:`hc-code`,rows:`10`,spellcheck:`false`,placeholder:`样式写在这里，会自动套用到左侧内容上`},null,512),[[g,V.value]])]),h(`div`,O,[a[9]||=h(`span`,{class:`hc-pane-title`},`可插入变量`,-1),h(`div`,k,[(d(),m(s,null,t(K,e=>h(`button`,{key:e,type:`button`,class:`hc-var`,onClick:t=>ce(e)},r(e),9,A)),64))]),a[10]||=h(`p`,{class:`hc-tip`},[u(` 安全提示：`),h(`code`,null,`<script>`),u(`、`),h(`code`,null,`<iframe>`),u(`、 `),h(`code`,null,`<form>`),u(` 会被自动移除，事件属性也会被清掉。 CSS 里写 `),h(`code`,null,`position: fixed`),u(` 会被降级为 `),h(`code`,null,`relative`),u(`， 避免自定义内容盖住整个页面。 `)],-1)]),h(`div`,j,[h(`div`,M,[a[12]||=h(`span`,{class:`hc-pane-title`},`不会写？让 AI 帮你写`,-1),o(y,{size:`sm`,onClick:q},{default:i(()=>[...a[11]||=[u(`复制提示词`,-1)]]),_:1})]),a[13]||=h(`p`,{class:`hc-ai-lead`},[u(` 点「复制提示词」→ 粘给任意 AI → 把它返回的 `),h(`code`,null,`html`),u(` 和 `),h(`code`,null,`css`),u(` 两个代码块，分别粘进上面对应的框 → 右侧预览满意后点「保存并发布」。 `)],-1),h(`pre`,{class:`hc-ai-preview`},r(I))])]),_:1})]),h(`div`,N,[o(b,{padding:!1},{default:i(()=>[a[17]||=h(`div`,{class:`hc-pane-head`},[h(`span`,{class:`hc-pane-title`},`实时预览`),h(`span`,{class:`hc-size`},`公开首页实际效果`)],-1),h(`div`,P,[$.value?(d(),l(S,{key:0,html:B.value,css:V.value,"style-id":`ch-custom-home-preview-style`},null,8,[`html`,`css`])):(d(),m(`div`,F,[a[15]||=h(`p`,{class:`hc-starter-title`},`当前使用默认首页`,-1),a[16]||=h(`p`,{class:`hc-starter-sub`},`左侧 HTML 为空时，公开首页显示面板自带的默认内容。`,-1),o(y,{variant:`secondary`,size:`sm`,onClick:a[2]||=e=>{B.value=L,V.value=R}},{default:i(()=>[...a[14]||=[u(` 填入起步模板 `,-1)]]),_:1})]))])]),_:1})])])]))}},[[`__scopeId`,`data-v-9ef8cf6e`]]);export{z as default};