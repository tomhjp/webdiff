(function(){
  // Stream page: a dedicated full-window tail of one tmux pane. We
  // detect it by the presence of #pane-tail (rendered server-side by
  // writeStreamPage) and run a self-contained handler. The diff/listing
  // logic below shouldn't run here — there are no <details> on this
  // page anyway, but bailing early keeps the two flows independent.
  var streamRoot=document.getElementById('pane-tail');
  if(streamRoot){setupStreamPage(streamRoot);return}

  function setupStreamPage(root){
    var paneID=root.dataset.paneId;
    var inner=root.querySelector('.term-inner');
    var jumpBtn=document.getElementById('jump-bottom');
    // Per-line buffer mirroring the server's `lastLines` state. Each
    // entry is one pre-wrapped <span class="line">...</span> string;
    // we never reparse it, just join + assign to innerHTML.
    var paneLines=[];
    // `follow` tracks whether we should auto-stick the viewport to the
    // bottom on each new snapshot. It's driven entirely by the user's
    // scroll position: at the bottom → follow on, scrolled up → off.
    // We don't need a separate "user vs programmatic scroll" flag
    // because the programmatic scroll always lands at the bottom,
    // which keeps follow true — the same outcome we want.
    var follow=true;
    function nearBottom(){
      var doc=document.documentElement;
      return (window.innerHeight+window.scrollY)>=(doc.scrollHeight-30);
    }
    function scrollToBottom(){
      window.scrollTo(0,document.documentElement.scrollHeight);
    }
    function refreshFollowUI(){
      if(jumpBtn)jumpBtn.hidden=follow;
    }
    window.addEventListener('scroll',function(){
      follow=nearBottom();
      refreshFollowUI();
    },{passive:true});
    if(jumpBtn){
      jumpBtn.addEventListener('click',function(){
        follow=true;
        scrollToBottom();
        refreshFollowUI();
      });
    }
    // tagDiffBgLines: walks the freshly-rendered rows and stamps a
    // line-bg-add / line-bg-del class on any `.line` whose content
    // carries an ANSI green or red bg span. The CSS for those classes
    // paints the colour on the .line block (filling the full line-
    // height) and zeroes out the inner inline bg, so adjacent diff
    // rows form a solid block without the leading-gap or descender-
    // clipping artefacts inline bg leaves behind.
    function tagDiffBgLines(){
      var rows=inner.querySelectorAll('.line');
      for(var i=0;i<rows.length;i++){
        var row=rows[i];
        row.classList.remove('line-bg-add','line-bg-del');
        if(row.querySelector('.term-bgx22, .term-bg42')){
          row.classList.add('line-bg-add');
        }else if(row.querySelector('.term-bgx52, .term-bg41')){
          row.classList.add('line-bg-del');
        }
      }
    }
    function applyUpdate(msg){
      if(msg.reset){
        paneLines=msg.append?msg.append.slice():[];
      }else{
        var drop=msg.drop||0;
        var append=msg.append||[];
        if(drop>0){
          paneLines.splice(0,drop);
          // streamDelta on the server falls back to drop=oldLen,
          // append=newLen whenever it can't align old to new — i.e.
          // any time a row in the existing buffer changed in place
          // (cursor blink, spinner, status redraw). Ripping out every
          // child and regenerating would nuke the user's text
          // selection on every blink. Instead, only physically remove
          // children when this is a genuine top shift (drop strictly
          // less than what's on screen); otherwise leave the DOM
          // alone and let syncDOM swap only the rows that really
          // differ.
          if(drop<inner.children.length){
            for(var k=0;k<drop;k++){
              if(inner.firstChild)inner.firstChild.remove();
            }
          }
        }
        if(append.length)paneLines.push.apply(paneLines,append);
      }
      syncDOM();
      tagDiffBgLines();
      if(follow)scrollToBottom();
    }
    // Reconcile DOM children against paneLines: rows whose HTML hasn't
    // changed keep their existing DOM node (and therefore any active
    // text selection inside them); rows that differ get swapped in
    // place; trailing extras are dropped. _lineHTML caches the string
    // we built each child from so the comparison stays cheap and isn't
    // affected by any browser-side HTML normalisation of outerHTML.
    function syncDOM(){
      var children=inner.children;
      for(var i=0;i<paneLines.length;i++){
        var html=paneLines[i];
        var existing=children[i];
        if(existing&&existing._lineHTML===html)continue;
        var tpl=document.createElement('div');
        tpl.innerHTML=html;
        var node=tpl.firstChild;
        if(!node)continue;
        node._lineHTML=html;
        if(existing){
          inner.replaceChild(node,existing);
        }else{
          inner.appendChild(node);
        }
      }
      while(inner.children.length>paneLines.length){
        inner.lastChild.remove();
      }
    }
    // Reconnect with capped exponential backoff. The server's first
    // message on every fresh connection is {reset:true, append:[…]},
    // so the client comes back in sync without sequence numbers or
    // resume tokens. Triggers we care about:
    //   • deploy / restart — server briefly down, onerror fires
    //   • mobile idle      — the OS kills long-lived connections when
    //                        the tab is backgrounded, and the next
    //                        read fails when the user returns
    //   • transient blip   — network drop, proxy timeout, etc.
    var es=null;
    var reconnectDelay=500;
    var reconnectTimer=null;
    function scheduleReconnect(){
      if(reconnectTimer)return;
      reconnectTimer=setTimeout(function(){
        reconnectTimer=null;
        reconnectDelay=Math.min(reconnectDelay*2,5000);
        connectStream();
      },reconnectDelay);
    }
    function connectStream(){
      if(es){try{es.close()}catch(e){}}
      es=new EventSource('/api/pane/stream?paneID='+encodeURIComponent(paneID));
      es.onopen=function(){reconnectDelay=500};
      es.onmessage=function(ev){
        try{
          var msg=JSON.parse(ev.data);
          if(msg.error){inner.textContent='stream error: '+msg.error;return}
          applyUpdate(msg);
        }catch(e){}
      };
      es.onerror=function(){
        if(es){try{es.close()}catch(e){}es=null}
        scheduleReconnect();
      };
    }
    connectStream();
    // Force-reconnect when the tab becomes visible again. Mobile
    // browsers don't always flip EventSource.readyState from OPEN to
    // CLOSED until the next failed read, so we can't trust it alone
    // to notice that the suspended-tab connection has been killed.
    // If readyState says OPEN we leave it alone — onerror will fire
    // (and reconnect) on the next read attempt if it's actually dead.
    document.addEventListener('visibilitychange',function(){
      if(document.visibilityState!=='visible')return;
      if(es&&es.readyState===1)return;
      if(reconnectTimer){clearTimeout(reconnectTimer);reconnectTimer=null}
      reconnectDelay=500;
      connectStream();
    });
    refreshFollowUI();

    // Footer toolbar: ↑/↓/enter/send wire into POST /api/pane/input.
    // The input field re-uses the message-send handler so Enter inside
    // the textbox submits the same way the explicit "send" button
    // does. After any successful interaction we re-stick to the
    // bottom — the user just nudged the pane and almost certainly
    // wants to see the response.
    function postInput(payload){
      return fetch('/api/pane/input',{
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify(Object.assign({paneID:paneID},payload))
      }).then(function(r){
        if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
        follow=true;refreshFollowUI();scrollToBottom();
      }).catch(function(e){console.warn('pane input failed:',e)});
    }
    Array.prototype.forEach.call(document.querySelectorAll('.stream-key'),function(b){
      b.addEventListener('click',function(){postInput({key:b.dataset.key})});
    });
    var msgInput=document.getElementById('pane-msg');
    var sendMsgBtn=document.getElementById('pane-send');
    function sendMessage(){
      var t=msgInput.value;
      if(!t)return;
      postInput({text:t}).then(function(){
        msgInput.value='';
        msgInput.style.height='auto';
      });
    }
    if(sendMsgBtn)sendMsgBtn.addEventListener('click',sendMessage);
    if(msgInput){
      msgInput.addEventListener('keydown',function(ev){
        if(ev.key==='Enter'&&!ev.shiftKey){ev.preventDefault();sendMessage()}
      });
      msgInput.addEventListener('input',function(){
        msgInput.style.height='auto';
        msgInput.style.height=msgInput.scrollHeight+'px';
      });
    }
    // Restart wires through to /api/pane/restart, which kills the
    // current tmux session. After the kill we navigate to the same
    // /_/claude<repoURL> the diff page's claude button hits — that
    // handler spawns a fresh session and 303s back to the stream view.
    var restartBtn=document.getElementById('restart-session');
    if(restartBtn){
      restartBtn.addEventListener('click',function(){
        if(!window.confirm('Kill the tmux session and start a fresh one? Scrollback will be lost.'))return;
        var repoURL=restartBtn.dataset.repoUrl||'/';
        restartBtn.disabled=true;
        fetch('/api/pane/restart',{
          method:'POST',
          headers:{'Content-Type':'application/json'},
          body:JSON.stringify({paneID:paneID})
        }).then(function(r){
          if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
          window.location.href='/_/claude'+repoURL;
        }).catch(function(e){
          restartBtn.disabled=false;
          window.alert('restart failed: '+e.message);
        });
      });
    }
    // Keep #pane-tail padding-bottom in sync with the toolbar height so
    // auto-follow never hides the last line under the fixed toolbar.
    var toolbar=document.querySelector('.stream-toolbar');
    function syncPad(){root.style.paddingBottom=(toolbar?toolbar.offsetHeight+16:100)+'px'}
    if(toolbar&&typeof ResizeObserver!=='undefined'){
      new ResizeObserver(syncPad).observe(toolbar);
    }
    syncPad();
  }

  // Listing page: poll for spinning *-wd sessions and reflect that
  // state on the tmux rows with a CSS spinner via .session-spinning.
  var tmuxRows=document.querySelectorAll('.tmux-row[data-session]');
  if(tmuxRows.length>0){
    function pollSpinning(){
      fetch('/api/sessions/spinning')
        .then(function(r){return r.json()})
        .then(function(spinning){
          tmuxRows.forEach(function(row){
            row.classList.toggle('session-spinning',!!spinning[row.dataset.session]);
          });
        }).catch(function(){});
    }
    pollSpinning();
    setInterval(pollSpinning,2000);
  }

  // Diff-mode persistence lives entirely client-side: localStorage
  // remembers the user's last pick, and we redirect-on-load when the URL
  // doesn't already have a `?mode=…` so the rendered page matches.
  // Server defaults to HEAD (working) when the URL has no mode, which
  // means a fresh URL paste / external link sees the simple default.
  var MODE_KEY='webdiff:diffMode';
  function loadStoredMode(){try{return localStorage.getItem(MODE_KEY)||''}catch(e){return ''}}
  function saveStoredMode(m){
    try{
      if(!m||m==='working')localStorage.removeItem(MODE_KEY);
      else localStorage.setItem(MODE_KEY,m);
    }catch(e){}
  }
  var pageURL=new URL(location.href);
  var urlMode=pageURL.searchParams.get('mode');
  var storedMode=loadStoredMode();
  if(!urlMode&&storedMode&&storedMode!=='working'){
    pageURL.searchParams.set('mode',storedMode);
    location.replace(pageURL.toString());
    return;
  }
  if(urlMode)saveStoredMode(urlMode);
  var modeSelect=document.getElementById('diff-mode-select');
  if(modeSelect){
    modeSelect.addEventListener('change',function(){
      saveStoredMode(modeSelect.value);
      var u=new URL(location.href);
      if(modeSelect.value==='working')u.searchParams.delete('mode');
      else u.searchParams.set('mode',modeSelect.value);
      // Switching modes resets the per-view "full context" toggle —
      // most users picking a different base want to see the default
      // 3-line diff, not whatever expanded view they had open before.
      u.searchParams.delete('context');
      location.href=u.toString();
    });
  }

  // "full context" toggle: re-renders the current diff page with
  // git's -U99999 flag (server side) so each file shows its full
  // contents with hunks inlined. State lives in the URL; not
  // threaded into outgoing nav links because full context only makes
  // sense for the current view.
  var ctxBtn=document.getElementById('toggle-context');
  if(ctxBtn){
    ctxBtn.addEventListener('click',function(){
      var u=new URL(location.href);
      if(ctxBtn.dataset.full==='true')u.searchParams.delete('context');
      else u.searchParams.set('context','full');
      location.href=u.toString();
    });
  }

  // Sticky stack: the page heading is pinned at top:0; the pillbar
  // sticks under it; each <details>' summary sticks under the pillbar.
  // CSS reads --page-heading-h and --pillbar-h to compute the offsets.
  // We set them from measured rendered heights so the stack still lines
  // up if the heading wraps onto extra rows on a narrow viewport or
  // someone bumps the font-size.
  function syncStickyOffsets(){
    var h=document.querySelector('.page-heading');
    var p=document.querySelector('.pillbar');
    var root=document.documentElement;
    if(h)root.style.setProperty('--page-heading-h',h.offsetHeight+'px');
    if(p)root.style.setProperty('--pillbar-h',p.offsetHeight+'px');
  }
  syncStickyOffsets();
  if(typeof ResizeObserver!=='undefined'){
    var stickyRO=new ResizeObserver(syncStickyOffsets);
    var heading=document.querySelector('.page-heading');
    var pillbar=document.querySelector('.pillbar');
    if(heading)stickyRO.observe(heading);
    if(pillbar)stickyRO.observe(pillbar);
  }
  window.addEventListener('resize',syncStickyOffsets);

  var FILE_KEY='webdiff:fileState';
  var details=document.querySelectorAll('details[id]');
  var btn=document.getElementById('toggle-all');
  if(details.length===0)return;

  function load(){try{return JSON.parse(localStorage.getItem(FILE_KEY)||'null')}catch(e){return null}}
  function save(s){try{localStorage.setItem(FILE_KEY,JSON.stringify(s))}catch(e){}}

  var state=load()||{};
  details.forEach(function(d){if(state[d.id]===false)d.open=false;});

  function anyOpen(){return Array.from(details).some(function(d){return d.open})}
  function refreshBtn(){if(btn)btn.textContent=anyOpen()?'collapse all':'expand all'}
  details.forEach(function(d){
    d.addEventListener('toggle',function(){
      state[d.id]=d.open;save(state);refreshBtn();
    });
  });
  refreshBtn();

  function toggleAll(){
    var any=anyOpen();
    details.forEach(function(d){d.open=!any});
  }
  if(btn)btn.addEventListener('click',toggleAll);

  document.querySelectorAll('.pill').forEach(function(p){
    p.addEventListener('click',function(){
      var id=p.getAttribute('href').slice(1);
      var el=document.getElementById(id);
      if(el&&!el.open)el.open=true;
    });
  });

  var pills=document.querySelectorAll('.pill');
  function updateActive(){
    var scrollY=window.scrollY+60;var active=null;
    details.forEach(function(d,i){if(d.offsetTop<=scrollY)active=i});
    pills.forEach(function(p,i){p.classList.toggle('pill-active',i===active)});
    if(active!==null){
      var ap=pills[active];var bar=ap.parentElement;
      bar.scrollTo({left:ap.offsetLeft-bar.offsetLeft-8,behavior:'smooth'});
    }
  }
  window.addEventListener('scroll',updateActive,{passive:true});
  requestAnimationFrame(function(){requestAnimationFrame(updateActive)});

  // ─── review comments → claude ──────────────────────────────────────
  var REPO_PATH=window.location.pathname.replace(/\/+$/,'')||'/';
  var COMMENT_KEY='webdiff:comments:'+REPO_PATH;
  var sendBtn=document.getElementById('send-comments');
  var sendCount=sendBtn&&sendBtn.querySelector('.send-count');

  function loadComments(){try{return JSON.parse(localStorage.getItem(COMMENT_KEY)||'[]')}catch(e){return []}}
  function saveComments(arr){try{localStorage.setItem(COMMENT_KEY,JSON.stringify(arr))}catch(e){}}
  function clearComments(){try{localStorage.removeItem(COMMENT_KEY)}catch(e){}}

  function refreshSendBtn(){
    var n=loadComments().length;
    if(sendCount)sendCount.textContent=String(n);
    // sendBtn stays visible at all times now — it's the only entry
    // point to the modal, which owns the "+ overall review" composer.
  }

  function commentKey(c){return c.file+'\u0001'+(c.newLine||'')+'\u0001'+(c.oldLine||'')+'\u0001'+c.ts}

  function rangeEnd(s){if(!s)return '';var i=s.indexOf('-');return i>=0?s.slice(i+1):s}

  function findLineEl(c){
    var fileSel='.line[data-file="'+CSS.escape(c.file)+'"]';
    var endNew=rangeEnd(c.newLine);
    var endOld=rangeEnd(c.oldLine);
    if(endNew){
      var el=document.querySelector(fileSel+'[data-new-line="'+endNew+'"]');
      if(el)return el;
    }
    if(endOld){
      var el=document.querySelector(fileSel+'[data-old-line="'+endOld+'"]');
      if(el)return el;
    }
    return document.querySelector(fileSel);
  }

  // findAnchor returns the DOM node the bubble (or its replacement
  // composer when editing) should be inserted after. Two scopes are
  // anchored on the page; review-level (no file) lives only in the
  // modal now and has no on-page anchor.
  //   file    — the line-file-anchor span (first quiet line under the
  //             summary); bubbles stack directly below it inside the
  //             term-inner, the same way line-level bubbles do
  //   line    — the diff line itself (existing line-comment behaviour)
  function findAnchor(c){
    if(!c.file)return null;
    if(!c.oldLine&&!c.newLine){
      var anchor=document.querySelector('.line-file-anchor[data-file-comment="'+CSS.escape(c.file)+'"]');
      if(anchor)return anchor;
      // Fallback: file lacks a quiet first line (very short binary
      // diffs etc). Drop back to the summary so the bubble still
      // renders somewhere sensible.
      var details=document.querySelector('details[data-file="'+CSS.escape(c.file)+'"]');
      return details?details.querySelector('summary'):null;
    }
    return findLineEl(c);
  }

  function renderBubble(c,anchor){
    if(!anchor)return;
    var bubble=document.createElement('span');
    bubble.className='comment-bubble';
    bubble.dataset.key=commentKey(c);
    var body=document.createElement('span');body.className='comment-body';body.textContent=c.text;
    var edit=document.createElement('button');edit.className='comment-edit';edit.type='button';edit.textContent='✎';
    edit.title='edit this comment';
    edit.addEventListener('click',function(ev){
      ev.stopPropagation();ev.preventDefault();
      // If a composer is already attached to this anchor, leave it
      // alone (and leave the bubble in place) — the user is mid-edit
      // on something else and we shouldn't silently swallow this click.
      if(anchor.nextElementSibling&&anchor.nextElementSibling.classList.contains('comment-composer'))return;
      bubble.remove();
      openComposer(null,c);
    });
    var del=document.createElement('button');del.className='comment-del';del.type='button';del.textContent='✕';
    del.title='delete this comment';
    del.addEventListener('click',function(ev){
      ev.stopPropagation();ev.preventDefault();
      var arr=loadComments().filter(function(x){return commentKey(x)!==commentKey(c)});
      saveComments(arr);bubble.remove();refreshSendBtn();
    });
    bubble.appendChild(body);bubble.appendChild(edit);bubble.appendChild(del);
    anchor.insertAdjacentElement('afterend',bubble);
  }

  function renderAllBubbles(){
    document.querySelectorAll('.comment-bubble').forEach(function(b){b.remove()});
    // Iterate in reverse so each `insertAdjacentElement('afterend')`
    // pushes earlier bubbles down — final on-screen order matches
    // storage order (oldest closest to the anchor, newest furthest).
    loadComments().slice().reverse().forEach(function(c){renderBubble(c,findAnchor(c))});
  }

  function fmtRange(nums){
    if(nums.length===0)return '';
    var min=nums[0],max=nums[0];
    for(var i=1;i<nums.length;i++){if(nums[i]<min)min=nums[i];if(nums[i]>max)max=nums[i]}
    return min===max?String(min):(min+'-'+max);
  }

  // openComposer renders the inline textarea+save/cancel UI for any
  // of the on-page comment scopes. Two ways to call it:
  //
  //   openComposer(spec, null)       — new comment. spec is one of:
  //     { kind:'line',   range:[lineEl, …] }
  //     { kind:'file',   file:'path/to/x.go', anchor: <summary> }
  //
  //   openComposer(null, existing)   — edit. The bubble must have
  //     already been removed by the caller; abort() restores it. The
  //     anchor is derived from `existing` via findAnchor(). Editing a
  //     review-level comment hits this path with no anchor — handled
  //     at the call site (modal row's edit affordance).
  function openComposer(spec,existing){
    var anchor,file,placeholder='leave a review comment…';
    var newNums=[],oldNums=[];
    if(existing){
      anchor=findAnchor(existing);
      file=existing.file;
    }else if(spec){
      if(spec.kind==='line'){
        if(!spec.range||spec.range.length===0)return;
        var last=spec.range[spec.range.length-1];
        anchor=last;
        file=last.dataset.file||'';
        spec.range.forEach(function(l){
          if(l.dataset.file!==file)return;
          if(l.dataset.newLine)newNums.push(parseInt(l.dataset.newLine,10));
          if(l.dataset.oldLine)oldNums.push(parseInt(l.dataset.oldLine,10));
        });
        if(spec.range.length>1)placeholder='comment on '+spec.range.length+' lines…';
      }else if(spec.kind==='file'){
        anchor=spec.anchor;file=spec.file;placeholder='comment on '+file+'…';
      }
    }
    if(!anchor)return;
    if(!anchor)return;
    if(anchor.nextElementSibling&&anchor.nextElementSibling.classList.contains('comment-composer'))return;
    var comp=document.createElement('span');comp.className='comment-composer';
    var ta=document.createElement('textarea');ta.rows=2;
    if(existing){ta.value=existing.text}
    else{ta.placeholder=placeholder}
    var actions=document.createElement('span');actions.className='comment-actions';
    var save=document.createElement('button');save.type='button';save.className='toggle';save.textContent='save';
    var cancel=document.createElement('button');cancel.type='button';cancel.className='toggle';cancel.textContent='cancel';
    actions.appendChild(save);actions.appendChild(cancel);
    comp.appendChild(ta);comp.appendChild(actions);
    anchor.insertAdjacentElement('afterend',comp);
    ta.focus();
    if(existing)ta.setSelectionRange(ta.value.length,ta.value.length);
    function close(){comp.remove()}
    function abort(){close();if(existing)renderBubble(existing,anchor)}
    cancel.addEventListener('click',abort);
    save.addEventListener('click',function(){
      var t=ta.value.trim();
      if(!t){abort();return}
      if(existing){
        if(t===existing.text){abort();return}
        var key=commentKey(existing);
        var arr=loadComments();var updated=null;
        for(var i=0;i<arr.length;i++){
          if(commentKey(arr[i])===key){
            arr[i]={file:arr[i].file,oldLine:arr[i].oldLine,newLine:arr[i].newLine,text:t,ts:arr[i].ts};
            updated=arr[i];
          }
        }
        saveComments(arr);close();
        if(updated)renderBubble(updated,anchor);
      }else{
        var c={file:file,oldLine:fmtRange(oldNums),newLine:fmtRange(newNums),text:t,ts:Date.now()};
        var arr=loadComments();arr.push(c);saveComments(arr);
        close();renderBubble(c,anchor);refreshSendBtn();
      }
    });
    ta.addEventListener('keydown',function(ev){
      if((ev.metaKey||ev.ctrlKey)&&ev.key==='Enter'){save.click()}
      else if(ev.key==='Escape'){abort()}
    });
  }

  // Drag-to-select multi-line comments. mousedown anchors the start,
  // mousemove paints the live range, mouseup opens the composer for
  // whatever was selected. A click without movement is just a 1-line
  // range, so this also covers the simple "click a line to comment"
  // path — there's no separate single-click handler.
  var dragStart=null;var dragSelected=[];

  function lineRange(start,end){
    var inner=start.closest('.term-inner');
    if(!inner||end.closest('.term-inner')!==inner)return [start];
    var all=Array.from(inner.querySelectorAll('.line[data-file]'));
    var i1=all.indexOf(start),i2=all.indexOf(end);
    if(i1<0||i2<0)return [start];
    if(i2<i1){var t=i1;i1=i2;i2=t;}
    return all.slice(i1,i2+1);
  }

  function clearDragHighlight(){
    dragSelected.forEach(function(l){l.classList.remove('line-drag-selected')});
    dragSelected=[];
  }

  document.addEventListener('mousedown',function(ev){
    if(ev.button!==0)return;
    if(ev.target.closest('.comment-bubble,.comment-composer,.comment-del,.toggle,a,button,input,textarea'))return;
    var line=ev.target.closest('.line[data-file]');
    if(!line)return;
    ev.preventDefault();
    dragStart=line;
    clearDragHighlight();
    dragSelected=[line];
    line.classList.add('line-drag-selected');
  });

  document.addEventListener('mousemove',function(ev){
    if(!dragStart)return;
    var hover=ev.target.closest('.line[data-file]');
    if(!hover)return;
    var newRange=lineRange(dragStart,hover);
    if(newRange.length===dragSelected.length&&newRange[newRange.length-1]===dragSelected[dragSelected.length-1])return;
    dragSelected.forEach(function(l){l.classList.remove('line-drag-selected')});
    dragSelected=newRange;
    newRange.forEach(function(l){l.classList.add('line-drag-selected')});
  });

  document.addEventListener('mouseup',function(){
    if(!dragStart)return;
    var range=dragSelected.slice();
    clearDragHighlight();
    dragStart=null;
    if(range.length>0)openComposer({kind:'line',range:range});
  });

  // Per-file comment trigger: the first quiet line under the file's
  // <summary> carries `data-file-comment`. Clicking it opens a
  // file-scope composer — same UX shape as a line-level click, just
  // anchored to the whole file. The drag-select handlers above only
  // engage on `.line[data-file]`, so this listener doesn't fight them.
  Array.prototype.forEach.call(document.querySelectorAll('.line-file-anchor'),function(el){
    el.addEventListener('click',function(ev){
      ev.preventDefault();ev.stopPropagation();
      openComposer({kind:'file',anchor:el,file:el.dataset.fileComment||''},null);
    });
  });

  renderAllBubbles();
  refreshSendBtn();

  function flashStatus(msg,kind){
    var n=document.createElement('span');n.className='send-status send-status-'+(kind||'info');n.textContent=msg;
    // Insert before the button so as the wrapper grows the button
    // stays anchored at the right edge (right:8px) and the status
    // text expands leftward into the page.
    sendBtn.insertAdjacentElement('beforebegin',n);
    setTimeout(function(){n.remove()},3500);
  }

  function postSend(){
    return fetch('/api/comments/send',{
      method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({repo:REPO_PATH,comments:loadComments()})
    }).then(function(r){return r.json().then(function(j){return {status:r.status,body:j}})});
  }

  function trySend(){
    postSend().then(function(res){
      if(res.status===200){
        clearComments();
        document.querySelectorAll('.comment-bubble').forEach(function(b){b.remove()});
        refreshSendBtn();
        // Each repo maps 1-1 to its tmux pane, so the stream URL is
        // just /_/stream<repoPath> — no paneID or label to thread
        // through.
        var path=window.location.pathname;
        if(path.slice(-1)!=='/')path+='/';
        window.location.href='/_/stream'+path;
        return;
      }
      flashStatus(res.body.error||('error '+res.status),'err');
    }).catch(function(e){flashStatus(String(e),'err')});
  }

  // Comments modal — a pre-flight summary of everything queued for
  // send, with per-row delete and a click-to-jump that scrolls (and
  // expands the file if collapsed) back to the bubble. Built lazily on
  // open so deletes elsewhere on the page don't have to keep a static
  // modal in sync. The footer carries the actual confirm-send button:
  // the floating bottom-right chip just opens this modal, so a stray
  // mobile tap reviews rather than firing the whole batch off to claude.
  function commentLoc(c){
    if(!c.file)return '(overall review)';
    if(c.newLine)return c.file+':'+c.newLine;
    if(c.oldLine)return c.file+' (-'+c.oldLine+')';
    return c.file;
  }
  function openCommentsModal(){
    var existing=document.querySelector('.modal-backdrop');
    if(existing){existing.remove();return}
    var backdrop=document.createElement('div');backdrop.className='modal-backdrop';
    var modal=document.createElement('div');modal.className='modal';
    var header=document.createElement('div');header.className='modal-header';
    var title=document.createElement('span');
    var close=document.createElement('button');close.type='button';close.className='toggle';close.textContent='close';
    close.addEventListener('click',function(){backdrop.remove()});
    header.appendChild(title);header.appendChild(close);
    var bodyEl=document.createElement('div');bodyEl.className='modal-body';

    // Overall-review composer: replaces the page-level "+ overall
    // review comment" bar. Always present in the modal so review-level
    // remarks have a permanent home.
    var overall=document.createElement('div');overall.className='modal-overall';
    var overallTa=document.createElement('textarea');overallTa.placeholder='overall comment on this review…';overallTa.rows=2;
    var overallActions=document.createElement('div');overallActions.className='modal-overall-actions';
    var addBtn=document.createElement('button');addBtn.type='button';addBtn.className='toggle';addBtn.textContent='add';
    overallActions.appendChild(addBtn);
    overall.appendChild(overallTa);overall.appendChild(overallActions);

    var footer=document.createElement('div');footer.className='modal-footer';
    var confirm=document.createElement('button');confirm.type='button';confirm.className='toggle send-btn';
    confirm.addEventListener('click',function(){
      if(loadComments().length===0)return;
      backdrop.remove();trySend();
    });
    footer.appendChild(confirm);

    function refreshState(){
      var n=loadComments().length;
      title.textContent='review comments ('+n+')';
      confirm.textContent='send to claude ('+n+')';
      confirm.disabled=n===0;
    }
    function appendRow(c){
      var row=document.createElement('div');row.className='modal-row';
      var loc=document.createElement('span');loc.className='modal-loc';loc.textContent=commentLoc(c);
      var text=document.createElement('span');text.className='modal-text';text.textContent=c.text;
      var del=document.createElement('button');del.className='comment-del';del.type='button';del.textContent='✕';del.title='delete this comment';
      del.addEventListener('click',function(ev){
        ev.stopPropagation();ev.preventDefault();
        var k=commentKey(c);
        saveComments(loadComments().filter(function(x){return commentKey(x)!==k}));
        row.remove();renderAllBubbles();refreshSendBtn();refreshState();
      });
      row.addEventListener('click',function(){
        // Review-level rows have nowhere to jump to — leave the modal
        // open so the user can keep editing.
        if(!c.file)return;
        backdrop.remove();
        var details=document.querySelector('details[data-file="'+CSS.escape(c.file)+'"]');
        if(details&&!details.open)details.open=true;
        var anchor=findAnchor(c);
        if(anchor)anchor.scrollIntoView({behavior:'smooth',block:'center'});
      });
      row.appendChild(loc);row.appendChild(text);row.appendChild(del);
      bodyEl.appendChild(row);
    }
    function commitOverall(){
      var t=overallTa.value.trim();
      if(!t)return;
      var c={file:'',oldLine:'',newLine:'',text:t,ts:Date.now()};
      var arr=loadComments();arr.push(c);saveComments(arr);
      overallTa.value='';
      appendRow(c);refreshSendBtn();refreshState();
    }
    addBtn.addEventListener('click',commitOverall);
    overallTa.addEventListener('keydown',function(ev){
      if((ev.metaKey||ev.ctrlKey)&&ev.key==='Enter'){ev.preventDefault();commitOverall()}
    });

    loadComments().forEach(appendRow);
    refreshState();

    modal.appendChild(header);modal.appendChild(bodyEl);modal.appendChild(overall);modal.appendChild(footer);
    backdrop.appendChild(modal);
    backdrop.addEventListener('click',function(ev){if(ev.target===backdrop)backdrop.remove()});
    document.body.appendChild(backdrop);
  }
  document.addEventListener('keydown',function(ev){
    if(ev.key==='Escape'){
      var b=document.querySelector('.modal-backdrop');
      if(b)b.remove();
      return;
    }
    // Cmd/Ctrl+Enter (anywhere outside a textarea) and plain Enter
    // (only when nothing's focused — i.e. target is <body>) are the
    // "submit comments" shortcut: first press opens the review modal,
    // second press fires the send.
    //
    // We check ev.target rather than document.activeElement because
    // the composer's save handler removes the textarea from the DOM
    // synchronously; by the time the event bubbles up activeElement
    // is <body>, and the modal would then pop every time the user
    // saved a comment.
    //
    // Plain Enter is gated to target===body so we don't hijack
    // <details> summaries, pill links, expand-all/toggle-context
    // buttons, etc. — anything focusable already has Enter wired up.
    if(ev.key==='Enter'){
      var t=ev.target;
      if(t&&(t.tagName==='TEXTAREA'||t.tagName==='INPUT'))return;
      var modifier=ev.metaKey||ev.ctrlKey;
      if(!modifier&&t!==document.body)return;
      var modal=document.querySelector('.modal-backdrop');
      if(modal){
        var confirm=modal.querySelector('.send-btn');
        if(confirm){ev.preventDefault();confirm.click()}
        return;
      }
      if(sendBtn&&!sendBtn.hidden){
        ev.preventDefault();
        openCommentsModal();
      }
    }
  });
  if(sendBtn)sendBtn.addEventListener('click',openCommentsModal);
})();
