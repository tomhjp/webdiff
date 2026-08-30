(function(){
  // AGENT_NAME is the user-facing label of the agent webdiff spawns
  // (default "claude"; configurable via the server's -agent flag). It's
  // injected by the page template as a body data attribute so client
  // code can render button copy without a server roundtrip.
  var AGENT_NAME=(document.body&&document.body.dataset.agentName)||'claude';

  // Stream page: a dedicated full-window tail of one tmux pane. We
  // detect it by the presence of #pane-tail (rendered server-side by
  // writeStreamPage) and run a self-contained handler. The diff/listing
  // logic below shouldn't run here — there are no <details> on this
  // page anyway, but bailing early keeps the two flows independent.
  var streamRoot=document.getElementById('pane-tail');
  if(streamRoot){setupStreamPage(streamRoot);setupWorktreeRemove();return}

  // showHistoryModal is the shared "confirm, and here's the scrollback URL
  // to hand the next session" dialog. Both entry points need it: restart
  // (on the stream page, which archives then kills) and resume (on a
  // history entry, which only spawns) — they differ solely in the copy and
  // in what the confirm button does, so opts carries those.
  //
  // opts: {title, note, confirmLabel, historyURL, agents, onConfirm(done,
  // agent)}. The confirm handler gets a `done(err)` callback to re-enable
  // the button when its request fails, and the selected agent id when
  // opts.agents was supplied.
  //
  // opts.agents: {options:[id,…], selected} renders an agent drop-down
  // above the history URL. Omit it and the modal is exactly what it was
  // before — the resume flow on a history page has no agent to choose.
  function showHistoryModal(opts){
    var fullURL=opts.historyURL?(window.location.origin+opts.historyURL):'';
    var backdrop=document.createElement('div');backdrop.className='modal-backdrop';
    var modal=document.createElement('div');modal.className='modal';
    var header=document.createElement('div');header.className='modal-header';
    var title=document.createElement('span');title.textContent=opts.title;
    var close=document.createElement('button');close.type='button';close.className='toggle';close.textContent='cancel';
    close.addEventListener('click',function(){backdrop.remove()});
    header.appendChild(title);header.appendChild(close);
    var bodyEl=document.createElement('div');bodyEl.className='modal-body';
    var note=document.createElement('p');note.className='modal-note';
    note.textContent=opts.note;
    bodyEl.appendChild(note);
    var agentSel;
    if(opts.agents&&opts.agents.options&&opts.agents.options.length>0){
      var row=document.createElement('label');row.className='modal-agent-row';
      var lbl=document.createElement('span');lbl.textContent='agent';
      agentSel=document.createElement('select');agentSel.className='chrome-select';
      opts.agents.options.forEach(function(id){
        var o=document.createElement('option');o.value=id;o.textContent=id;
        if(id===opts.agents.selected)o.selected=true;
        agentSel.appendChild(o);
      });
      row.appendChild(lbl);row.appendChild(agentSel);
      bodyEl.appendChild(row);
    }
    var urlInput;
    if(fullURL){
      urlInput=document.createElement('input');urlInput.type='text';urlInput.readOnly=true;
      urlInput.className='history-url-input';urlInput.value=fullURL;
      bodyEl.appendChild(urlInput);
    }
    var footer=document.createElement('div');footer.className='modal-footer';
    if(fullURL){
      var copyBtn=document.createElement('button');copyBtn.type='button';copyBtn.className='toggle';copyBtn.textContent='copy';
      copyBtn.addEventListener('click',function(){
        function ok(){copyBtn.textContent='copied';setTimeout(function(){copyBtn.textContent='copy'},1500)}
        urlInput.focus();urlInput.select();
        if(navigator.clipboard&&navigator.clipboard.writeText){
          navigator.clipboard.writeText(fullURL).then(ok,function(){try{document.execCommand('copy');ok()}catch(e){}});
        }else{try{document.execCommand('copy');ok()}catch(e){}}
      });
      footer.appendChild(copyBtn);
    }
    var goBtn=document.createElement('button');goBtn.type='button';goBtn.className='toggle send-btn';goBtn.textContent=opts.confirmLabel;
    goBtn.addEventListener('click',function(){
      goBtn.disabled=true;
      opts.onConfirm(function(err){
        goBtn.disabled=false;
        if(err)window.alert(err.message);
      },agentSel?agentSel.value:'');
    });
    footer.appendChild(goBtn);
    modal.appendChild(header);modal.appendChild(bodyEl);modal.appendChild(footer);
    backdrop.appendChild(modal);
    backdrop.addEventListener('click',function(ev){if(ev.target===backdrop)backdrop.remove()});
    document.body.appendChild(backdrop);
    if(urlInput)setTimeout(function(){urlInput.focus();urlInput.select()},0);
  }

  // Worktree remove — rendered on both the worktree diff page and the
  // stream page for a worktree's pane. The stream page returns early
  // from the diff/listing flow below, so this has to be callable from
  // both paths rather than living inline in one of them.
  function setupWorktreeRemove(){
    var btn=document.getElementById('wt-remove-btn');
    if(!btn)return;
    btn.addEventListener('click',function(){
      var name=btn.dataset.name;
      if(!name)return;
      if(!window.confirm('Remove worktree "'+name+'"? Tmux session and uncommitted changes will be lost.'))return;
      btn.disabled=true;
      btn.textContent='removing…';
      fetch('/api/worktrees/remove',{
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify({name:name})
      }).then(function(r){
        if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
        location.href='/';
      }).catch(function(e){
        btn.disabled=false;
        btn.textContent='remove';
        window.alert('remove failed: '+e.message);
      });
    });
  }

  function setupStreamPage(root){
    var paneID=root.dataset.paneId;
    var inner=root.querySelector('.term-inner');
    var jumpBtn=document.getElementById('jump-bottom');
    // Per-line buffer mirroring the server's `lastLines` state. Each
    // entry is one pre-wrapped <span class="line">...</span> string;
    // syncDOM reconciles it without replacing unchanged rows.
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
    // Record the first visible row before an update. Mobile browsers do
    // their own scroll anchoring, but it is unreliable when a terminal TUI
    // redraws several rows at once. If this node survives reconciliation,
    // explicitly put it back at the same viewport position.
    function viewportAnchor(){
      if(follow)return null;
      var rows=inner.children;
      for(var i=0;i<rows.length;i++){
        var rect=rows[i].getBoundingClientRect();
        if(rect.bottom>0)return {node:rows[i],top:rect.top};
      }
      return null;
    }
    function restoreViewportAnchor(anchor){
      if(!anchor||!anchor.node.isConnected)return;
      var shift=anchor.node.getBoundingClientRect().top-anchor.top;
      if(Math.abs(shift)>.5)window.scrollBy(0,shift);
    }
    function applyUpdate(msg){
      var anchor=viewportAnchor();
      if(msg.reset){
        paneLines=msg.append?msg.append.slice():[];
      }else{
        var drop=msg.drop||0;
        var append=msg.append||[];
        if(drop>0){
          paneLines.splice(0,drop);
          // Remove a genuine shifted-off prefix now so syncDOM can retain
          // the unchanged rows after it (including the viewport anchor).
          // A full replacement stays in place until syncDOM swaps rows.
          if(drop<inner.children.length){
            for(var k=0;k<drop;k++){
              if(inner.firstChild)inner.firstChild.remove();
            }
          }
        }
        // `keep` makes live-tail redraws incremental. Pi inserts completed
        // output above its footer/status rows; the old drop+append protocol
        // treated that as a full-screen replacement. Missing keep means an
        // older server and retains the old drop+append semantics.
        var keep=(typeof msg.keep==='number')?msg.keep:paneLines.length;
        if(paneLines.length>keep)paneLines.splice(keep);
        if(append.length)paneLines.push.apply(paneLines,append);
      }
      syncDOM();
      tagDiffBgLines();
      if(follow)scrollToBottom();else restoreViewportAnchor(anchor);
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
    // Close the stream while the tab is hidden and reconnect when it
    // becomes visible again. webdiff is plain HTTP/1.1, so browsers cap
    // us at 6 connections to the host; a few backgrounded tabs each
    // holding an idle EventSource is enough to starve every new page
    // load, which shows up as the next tab hanging forever. Dropping
    // the stream on hide frees the slot, and the server's first message
    // on reconnect is a full {reset:true} snapshot so we lose nothing.
    document.addEventListener('visibilitychange',function(){
      if(document.visibilityState!=='visible'){
        if(reconnectTimer){clearTimeout(reconnectTimer);reconnectTimer=null}
        if(es){try{es.close()}catch(e){}es=null}
        return;
      }
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
    // resizeMsg keeps the textarea sized to its content up to the CSS
    // max-height (120px). We toggle overflow-y here rather than leaving it
    // `auto` in CSS because desktop browsers paint a scrollbar gutter for
    // an empty single-row textarea otherwise.
    function resizeMsg(){
      msgInput.style.height='auto';
      msgInput.style.height=msgInput.scrollHeight+'px';
      msgInput.style.overflowY=msgInput.scrollHeight>120?'auto':'hidden';
    }
    // Attachments staged for the next message. Each entry is the
    // server's response {path,name,bytes}. They're sent as a trailing
    // "Attachments:\n- <path>" appendix on the next pane input — keeps
    // big logs and binaries out of the tmux paste buffer while still
    // pointing the agent at something it can `cat` (or read with its
    // own file tools) on the sandbox.
    var attachments=[];
    var attachBox=document.getElementById('stream-attachments');
    function refreshAttachments(){
      if(!attachBox)return;
      attachBox.innerHTML='';
      if(attachments.length===0){attachBox.hidden=true;return}
      attachBox.hidden=false;
      attachments.forEach(function(a,idx){
        var chip=document.createElement('span');chip.className='attachment-chip';
        var label=document.createElement('span');label.className='attachment-name';
        label.textContent=a.name;
        label.title=a.path+' ('+formatBytes(a.bytes)+')';
        var x=document.createElement('button');x.type='button';x.className='attachment-del';x.textContent='×';
        x.title='remove this attachment';
        x.addEventListener('click',function(){
          attachments.splice(idx,1);refreshAttachments();
        });
        chip.appendChild(label);chip.appendChild(x);
        attachBox.appendChild(chip);
      });
      syncPad();
    }
    function formatBytes(n){
      if(!n&&n!==0)return '';
      if(n<1024)return n+' B';
      if(n<1024*1024)return (n/1024).toFixed(1)+' KB';
      return (n/(1024*1024)).toFixed(1)+' MB';
    }
    function sendMessage(){
      var t=msgInput.value;
      if(!t&&attachments.length===0)return;
      var body=t;
      if(attachments.length>0){
        // Trailing list — the agent reads the user message first, then
        // sees an "Attachments:" appendix it can fan out into reads.
        // Keeping it as a bare list (no other prose) means a re-prompt
        // doesn't accidentally restate the attachments.
        if(body)body+='\n\n';
        body+=attachments.length===1?'Attachment:\n- ':'Attachments:\n- ';
        body+=attachments.map(function(a){return a.path}).join('\n- ');
      }
      postInput({text:body}).then(function(){
        msgInput.value='';
        attachments=[];
        refreshAttachments();
        resizeMsg();
      });
    }
    if(sendMsgBtn)sendMsgBtn.addEventListener('click',sendMessage);
    if(msgInput){
      msgInput.addEventListener('keydown',function(ev){
        if(ev.key==='Enter'&&!ev.shiftKey){ev.preventDefault();sendMessage()}
      });
      msgInput.addEventListener('input',resizeMsg);
    }

    // Attach modal — paste a big blob of text or drop a file. The
    // upload posts JSON with base64-encoded contents (multipart would
    // need a CSRF carve-out in safeweb's config; sticking to JSON
    // keeps the API surface uniform). The 20 MB cap lives on the
    // server too — this is just a friendly client-side check.
    var ATTACH_MAX=20*1024*1024;
    var attachBtn=document.getElementById('pane-attach');
    function bytesToBase64(bytes){
      // chunk so a multi-MB attachment doesn't blow String.fromCharCode's
      // argument-count limit on some engines.
      var CHUNK=0x8000;var parts=[];
      for(var i=0;i<bytes.length;i+=CHUNK){
        parts.push(String.fromCharCode.apply(null,bytes.subarray(i,i+CHUNK)));
      }
      return btoa(parts.join(''));
    }
    function uploadAttachment(name,bytes){
      if(bytes.length>ATTACH_MAX){
        window.alert('Attachment too large: '+formatBytes(bytes.length)+' (max '+formatBytes(ATTACH_MAX)+')');
        return Promise.reject(new Error('too large'));
      }
      return fetch('/api/pane/attach',{
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify({paneID:paneID,name:name,contentBase64:bytesToBase64(bytes)})
      }).then(function(r){
        if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
        return r.json();
      }).then(function(j){
        attachments.push(j);refreshAttachments();return j;
      });
    }
    function readFileBytes(file){
      return new Promise(function(resolve,reject){
        var fr=new FileReader();
        fr.onerror=function(){reject(fr.error||new Error('read failed'))};
        fr.onload=function(){resolve(new Uint8Array(fr.result))};
        fr.readAsArrayBuffer(file);
      });
    }
    function pickFileName(prefix){
      var d=new Date();
      function pad(n){return n<10?'0'+n:n}
      return prefix+'-'+d.getFullYear()+pad(d.getMonth()+1)+pad(d.getDate())+'-'+pad(d.getHours())+pad(d.getMinutes())+pad(d.getSeconds())+'.txt';
    }
    function openAttachModal(){
      var backdrop=document.createElement('div');backdrop.className='modal-backdrop';
      var modal=document.createElement('div');modal.className='modal attach-modal';
      var header=document.createElement('div');header.className='modal-header';
      var title=document.createElement('span');title.textContent='attach context';
      var close=document.createElement('button');close.type='button';close.className='toggle';close.textContent='close';
      close.addEventListener('click',function(){backdrop.remove()});
      header.appendChild(title);header.appendChild(close);
      var bodyEl=document.createElement('div');bodyEl.className='modal-body';

      // Drop zone doubles as the paste target so the user can either
      // click "choose file", drop one onto the box, or paste text
      // straight in. The textarea inside it captures pasted text.
      var drop=document.createElement('div');drop.className='attach-drop';
      var hint=document.createElement('div');hint.className='attach-hint';
      hint.textContent='paste a large blob of text here, or drop / choose a file';
      var ta=document.createElement('textarea');ta.className='attach-text';ta.placeholder='(paste text here)';ta.rows=8;
      var nameRow=document.createElement('div');nameRow.className='attach-name-row';
      var nameLabel=document.createElement('label');nameLabel.textContent='filename:';nameLabel.className='attach-name-label';
      var nameInput=document.createElement('input');nameInput.type='text';nameInput.className='attach-name-input';
      nameInput.placeholder=pickFileName('paste');
      nameRow.appendChild(nameLabel);nameRow.appendChild(nameInput);
      var picker=document.createElement('input');picker.type='file';picker.className='attach-file-input';
      var pickBtn=document.createElement('button');pickBtn.type='button';pickBtn.className='toggle';pickBtn.textContent='choose file…';
      pickBtn.addEventListener('click',function(){picker.click()});
      drop.appendChild(hint);drop.appendChild(ta);drop.appendChild(nameRow);drop.appendChild(pickBtn);drop.appendChild(picker);
      bodyEl.appendChild(drop);

      var footer=document.createElement('div');footer.className='modal-footer';
      var status=document.createElement('span');status.className='attach-status';
      var addBtn=document.createElement('button');addBtn.type='button';addBtn.className='toggle send-btn';addBtn.textContent='attach';
      footer.appendChild(status);footer.appendChild(addBtn);

      function setBusy(msg){
        status.textContent=msg||'';addBtn.disabled=!!msg;pickBtn.disabled=!!msg;
      }
      function handleFile(file){
        setBusy('uploading '+file.name+'…');
        readFileBytes(file).then(function(bytes){
          return uploadAttachment(file.name,bytes);
        }).then(function(){
          backdrop.remove();
        }).catch(function(e){
          setBusy('');window.alert('attach failed: '+(e&&e.message||e));
        });
      }
      function handleText(){
        var t=ta.value;
        if(!t)return;
        var nm=(nameInput.value||'').trim()||pickFileName('paste');
        var enc=new TextEncoder().encode(t);
        setBusy('uploading…');
        uploadAttachment(nm,enc).then(function(){
          backdrop.remove();
        }).catch(function(e){
          setBusy('');window.alert('attach failed: '+(e&&e.message||e));
        });
      }
      addBtn.addEventListener('click',function(){
        if(picker.files&&picker.files[0]){handleFile(picker.files[0]);return}
        handleText();
      });
      picker.addEventListener('change',function(){
        if(picker.files&&picker.files[0])handleFile(picker.files[0]);
      });
      // Drop anywhere on the .attach-drop region. preventDefault on
      // dragover is what tells the browser the region accepts a drop.
      ['dragenter','dragover'].forEach(function(t){
        drop.addEventListener(t,function(ev){ev.preventDefault();drop.classList.add('attach-drop-over')});
      });
      ['dragleave','drop'].forEach(function(t){
        drop.addEventListener(t,function(ev){ev.preventDefault();drop.classList.remove('attach-drop-over')});
      });
      drop.addEventListener('drop',function(ev){
        var f=ev.dataTransfer&&ev.dataTransfer.files&&ev.dataTransfer.files[0];
        if(f)handleFile(f);
      });

      modal.appendChild(header);modal.appendChild(bodyEl);modal.appendChild(footer);
      backdrop.appendChild(modal);
      backdrop.addEventListener('click',function(ev){if(ev.target===backdrop)backdrop.remove()});
      document.body.appendChild(backdrop);
      // Defer focus a tick so the textarea captures the user's next
      // paste even if they triggered this via keyboard.
      setTimeout(function(){ta.focus()},0);
    }
    if(attachBtn)attachBtn.addEventListener('click',openAttachModal);
    // Restart pops a single confirm modal *before* the kill. Opening it
    // hits /api/pane/history-url, which archives the live scrollback and
    // returns its plain-text /history/<id> URL without touching the
    // session — so the modal can show a copyable history link for the next
    // session up front. The modal's confirm button is what actually kills
    // (via /api/pane/restart) and navigates to /agent<refURL>, the same
    // handler the diff page's agent button hits, which spawns a fresh
    // session and 303s back to the stream view.
    //
    // The modal's agent drop-down decides what the respawn runs: the
    // chosen id goes onto /agent<refURL> as ?agent=, which the server
    // treats as an allowlist lookup. It opens on the agent currently in
    // the pane (data-agent-id) — restarting without touching it keeps
    // the same agent, which is the common case.
    var restartBtn=document.getElementById('restart-session');
    if(restartBtn){
      restartBtn.addEventListener('click',function(){
        var repoURL=restartBtn.dataset.repoUrl||'/';
        var choices=(restartBtn.dataset.agentChoices||'').split(',').filter(Boolean);
        restartBtn.disabled=true;
        fetch('/api/pane/history-url',{
          method:'POST',
          headers:{'Content-Type':'application/json'},
          body:JSON.stringify({paneID:paneID})
        }).then(function(r){
          if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
          return r.json().catch(function(){return {}});
        }).then(function(j){
          restartBtn.disabled=false;
          var historyURL=j&&j.historyURL;
          showHistoryModal({
            title:'restart session',
            note:historyURL
              ?'The tmux session will be killed and a fresh one started. Its scrollback is archived first — copy this URL to give the next session its history:'
              :'The tmux session will be killed and a fresh one started.',
            confirmLabel:'kill & start fresh',
            historyURL:historyURL,
            agents:{options:choices,selected:restartBtn.dataset.agentId||''},
            onConfirm:function(done,agent){
              fetch('/api/pane/restart',{
                method:'POST',
                headers:{'Content-Type':'application/json'},
                body:JSON.stringify({paneID:paneID})
              }).then(function(r){
                if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
                window.location.href='/agent'+repoURL+(agent?'?agent='+encodeURIComponent(agent):'');
              }).catch(function(e){
                done(new Error('restart failed: '+e.message));
              });
            }
          });
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

  // History entry page: resume spawns a fresh agent session for the repo
  // this archive belongs to, showing the archived scrollback's URL first so
  // it can be pasted into the new session as context. No kill involved —
  // the old session is already gone, which is why the archive is offered.
  var resumeBtn=document.getElementById('history-resume-btn');
  if(resumeBtn){
    resumeBtn.addEventListener('click',function(){
      var repoURL=resumeBtn.dataset.repoUrl||'/';
      showHistoryModal({
        title:'resume session',
        note:'A fresh '+AGENT_NAME+' session will be started for this repo — copy this URL to give it the previous session’s history:',
        confirmLabel:'start '+AGENT_NAME,
        historyURL:resumeBtn.dataset.historyUrl||'',
        onConfirm:function(){window.location.href='/agent'+repoURL}
      });
    });
  }

  // Forget drops the archive's sticky flag so it stops being offered on the
  // home page. The scrollback stays readable under /history/, so this is
  // dismissal rather than deletion — hence no confirm prompt.
  var forgetBtn=document.getElementById('history-forget-btn');
  if(forgetBtn){
    forgetBtn.addEventListener('click',function(){
      var id=forgetBtn.dataset.id;
      if(!id)return;
      forgetBtn.disabled=true;
      forgetBtn.textContent='forgetting…';
      fetch('/api/history/forget',{
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify({id:id})
      }).then(function(r){
        if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
        forgetBtn.textContent='forgotten';
      }).catch(function(e){
        forgetBtn.disabled=false;
        forgetBtn.textContent='forget';
        window.alert('forget failed: '+e.message);
      });
    });
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

  // Agent picker beside the diff page's spawn button. Only rendered
  // when no session is running yet, and it retargets the sibling
  // anchor rather than navigating itself — the button stays a plain
  // link, and the choice is only committed when it's clicked. Changing
  // it doesn't reload, so the page's other agent labels keep naming the
  // default until the session actually starts.
  var agentSelect=document.getElementById('agent-select');
  var agentBtn=document.getElementById('agent-btn');
  if(agentSelect&&agentBtn){
    agentSelect.addEventListener('change',function(){
      var u=new URL(agentBtn.href,location.href);
      u.searchParams.set('agent',agentSelect.value);
      agentBtn.href=u.pathname+u.search;
      agentBtn.textContent=agentSelect.value;
      agentBtn.title='start a '+agentSelect.value+' session for this repo';
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

  // Sync buttons (push/pull) — relative POSTs land on the desktop
  // client's /_local/sync mux (the page is served via the desktop
  // reverse proxy, so same-origin = desktop). Brief visual feedback
  // states the request outcome without taking the user off the page.
  Array.prototype.forEach.call(document.querySelectorAll('button[data-sync-dir]'),function(b){
    var origLabel=b.textContent;
    b.addEventListener('click',function(){
      var dir=b.dataset.syncDir;
      var body={kind:b.dataset.kind,name:b.dataset.name};
      b.disabled=true;b.title='';
      b.classList.remove('sync-ok','sync-err');
      b.classList.add('sync-busy');
      b.textContent=(dir==='push'?'pushing…':'pulling…');
      fetch('/_local/sync/'+dir,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
        .then(function(r){
          if(!r.ok)return r.text().then(function(t){throw new Error(t||('status '+r.status))});
          b.classList.remove('sync-busy');b.classList.add('sync-ok');
          b.textContent=origLabel;
          setTimeout(function(){b.classList.remove('sync-ok');b.disabled=false},1200);
        })
        .catch(function(err){
          b.classList.remove('sync-busy');b.classList.add('sync-err');
          b.title=String(err&&err.message||err);
          b.textContent=origLabel;
          setTimeout(function(){b.classList.remove('sync-err');b.disabled=false},2500);
        });
    });
  });

  // Worktree create — repo diff page only. Clicking "+ worktree"
  // reveals an inline form; submitting POSTs to /api/worktrees/create
  // and navigates to the new worktree's diff page. The form stays
  // visually attached to the page heading rather than floating as a
  // modal so the iOS soft keyboard doesn't reflow the page out from
  // under the textbox.
  var wtNewBtn=document.getElementById('wt-new-btn');
  var wtRow=document.getElementById('wt-create-row');
  var wtBranch=document.getElementById('wt-branch');
  var wtFromMain=document.getElementById('wt-from-main');
  var wtSubmit=document.getElementById('wt-create-submit');
  var wtCancel=document.getElementById('wt-create-cancel');
  if(wtNewBtn&&wtRow){
    function openWt(){
      wtRow.hidden=false;
      if(wtBranch){wtBranch.focus();wtBranch.select()}
    }
    function closeWt(){wtRow.hidden=true;if(wtBranch)wtBranch.value=''}
    wtNewBtn.addEventListener('click',function(){wtRow.hidden?openWt():closeWt()});
    if(wtCancel)wtCancel.addEventListener('click',closeWt);
    function submitWt(){
      var branch=(wtBranch&&wtBranch.value||'').trim();
      var repo=wtRow.dataset.repo;
      if(!branch||!repo)return;
      if(wtSubmit){wtSubmit.disabled=true;wtSubmit.textContent='creating…'}
      fetch('/api/worktrees/create',{
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body:JSON.stringify({repo:repo,branch:branch,fromMain:wtFromMain&&wtFromMain.checked})
      }).then(function(r){
        if(!r.ok)return r.text().then(function(t){throw new Error(t||r.statusText)});
        return r.json();
      }).then(function(j){
        if(j&&j.url)location.href=j.url;
      }).catch(function(e){
        if(wtSubmit){wtSubmit.disabled=false;wtSubmit.textContent='create'}
        window.alert('create failed: '+e.message);
      });
    }
    if(wtSubmit)wtSubmit.addEventListener('click',submitWt);
    if(wtBranch){
      wtBranch.addEventListener('keydown',function(ev){
        if(ev.key==='Enter'){ev.preventDefault();submitWt()}
        else if(ev.key==='Escape'){ev.preventDefault();closeWt()}
      });
    }
  }

  setupWorktreeRemove();

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

  // File-browser breadcrumb: when the path overflows its scroller,
  // start at the right end — the filename is where the user is; the
  // repo root is a swipe away.
  var crumbRow=document.querySelector('.crumb-row');
  if(crumbRow)crumbRow.scrollLeft=crumbRow.scrollWidth;

  // Fit-width (file viewer): scale .term-container's font down until the
  // longest line fits the viewport. This is what pinch-zoom-out *feels*
  // like it should do but can't: the wide content lives inside an
  // overflow-x scroller while the page itself is clamped to the device
  // width, so zooming out just shrinks a device-width column and leaves
  // blank space. Scaling the font instead changes the actual laid-out
  // width. The measure→scale loop runs a few passes because glyph
  // metrics don't shrink perfectly linearly with font-size; the 6px
  // floor keeps a pathological 500-column line from producing invisible
  // text (at that point the horizontal scrollbar comes back, by design).
  var fitBtn=document.getElementById('fit-width');
  if(fitBtn){
    var fitBox=document.querySelector('.term-container');
    var fitOn=false;
    function applyFit(){
      if(!fitBox)return;
      fitBox.style.fontSize='';
      if(!fitOn)return;
      for(var i=0;i<4;i++){
        var avail=fitBox.clientWidth;
        var need=fitBox.scrollWidth;
        if(need<=avail)break;
        var cur=parseFloat(getComputedStyle(fitBox).fontSize)||13;
        var next=Math.max(6,cur*avail/need);
        fitBox.style.fontSize=next+'px';
        if(next<=6)break;
      }
    }
    fitBtn.addEventListener('click',function(){
      fitOn=!fitOn;
      fitBtn.textContent=fitOn?'1:1':'fit';
      applyFit();
    });
    window.addEventListener('resize',applyFit);
  }

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
  // When a file collapses while the user is scrolled past its top
  // (typically: read to the bottom of a long diff, then collapsed),
  // pin the just-closed summary at its sticky offset. Without this,
  // the document shrinks under a preserved scrollY and dumps the user
  // far below — disorienting because the heading they just acted on
  // is now off-screen.
  function stickyOffset(){
    var s=getComputedStyle(document.documentElement);
    var h=parseFloat(s.getPropertyValue('--page-heading-h'))||0;
    var p=parseFloat(s.getPropertyValue('--pillbar-h'))||0;
    return h+p;
  }
  details.forEach(function(d){
    d.addEventListener('toggle',function(){
      state[d.id]=d.open;save(state);refreshBtn();
      if(d.open)return;
      var off=stickyOffset();
      var target=d.getBoundingClientRect().top+window.scrollY-off;
      if(window.scrollY>target+1)window.scrollTo(0,Math.max(0,target));
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

  // ─── review comments → agent ───────────────────────────────────────
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
    // Lines split into gutter + code (delta) only start a comment when
    // the press lands on the gutter — the code column is left to native
    // text selection so it can be copied without the line numbers. Lines
    // with no gutter (raw `git diff`) accept a press anywhere, as before.
    if(line.querySelector('.line-gutter')&&!ev.target.closest('.line-gutter'))return;
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

  // Faithful multi-line copy. Each diff row is a block, and browsers
  // derive the newlines in copied text from block boundaries — but only
  // for blocks that hold selected text. A blank source line renders an
  // empty .line-tail (the line-number gutter is user-select:none), so it
  // contributes no text and its newline is silently dropped: `a`/blank/`b`
  // copies as "a\nb". We rebuild the plaintext ourselves from the
  // .line-tail of every row the selection touches, joined by \n, so blank
  // rows keep their newline. Only engages for multi-row selections inside
  // the diff body; single-row and chrome/bubble copies fall through to
  // native.
  function clippedText(range,el){
    var full=document.createRange();full.selectNodeContents(el);
    var out=document.createRange();out.selectNodeContents(el);
    if(range.compareBoundaryPoints(Range.START_TO_START,full)>0)out.setStart(range.startContainer,range.startOffset);
    if(range.compareBoundaryPoints(Range.END_TO_END,full)<0)out.setEnd(range.endContainer,range.endOffset);
    return out.toString();
  }
  document.addEventListener('copy',function(ev){
    var sel=window.getSelection();
    if(!sel||sel.isCollapsed||sel.rangeCount===0)return;
    var range=sel.getRangeAt(0);
    var anc=range.commonAncestorContainer;
    if(anc.nodeType===3)anc=anc.parentNode;
    if(!anc.closest)return;
    var inner=anc.closest('.term-inner');
    if(!inner)return;
    if(anc.closest('.comment-bubble,.comment-composer,textarea,input'))return;
    var rows=Array.prototype.filter.call(inner.querySelectorAll('.line'),function(l){return sel.containsNode(l,true)});
    if(rows.length<2)return;
    var text=rows.map(function(l){return clippedText(range,l.querySelector('.line-tail')||l)}).join('\n');
    ev.clipboardData.setData('text/plain',text);
    ev.preventDefault();
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
        // Each ref maps 1-1 to its tmux pane, so the stream URL is
        // just /stream<refPath> — no paneID or label to thread
        // through.
        var path=window.location.pathname;
        if(path.slice(-1)!=='/')path+='/';
        window.location.href='/stream'+path;
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
  // mobile tap reviews rather than firing the whole batch off to the agent.
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
      confirm.textContent='send to '+AGENT_NAME+' ('+n+')';
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

// Copy-to-clipboard for any [data-copy] element (repo/worktree rows on the
// home page, the file-browser breadcrumb). Runs on every page, so it's
// outside the diff-page IIFE above and doesn't reuse that flashStatus —
// feedback is the glyph itself swapping to ✓ for a moment, which needs no
// anchor element to position against.
//
// webdiff serves plain HTTP on a tailscale IP, which is not a secure
// context, so navigator.clipboard is undefined here and the execCommand
// path is the one that actually runs — it's the fallback in name only.
// execCommand('copy') copies the live selection, hence the offscreen
// textarea. We restore the previous selection afterwards so copying a path
// doesn't clobber a selection the user was building in the diff.
(function(){
  var btns=document.querySelectorAll('[data-copy]');
  if(!btns.length)return;

  function legacyCopy(text){
    var sel=window.getSelection();
    var prev=sel&&sel.rangeCount>0?sel.getRangeAt(0):null;
    var ta=document.createElement('textarea');
    ta.value=text;
    // Off-screen rather than hidden: display:none / visibility:hidden
    // elements can't hold a selection, so the copy would silently no-op.
    ta.style.position='fixed';ta.style.top='-1000px';ta.style.opacity='0';
    ta.setAttribute('readonly','');
    document.body.appendChild(ta);
    var ok=false;
    try{
      ta.focus();ta.setSelectionRange(0,ta.value.length);
      ok=document.execCommand('copy');
    }catch(e){}
    ta.remove();
    if(prev&&sel){sel.removeAllRanges();sel.addRange(prev)}
    return ok;
  }

  Array.prototype.forEach.call(btns,function(btn){
    var text=btn.getAttribute('data-copy');
    var revert=null;
    function done(ok){
      var orig=btn.dataset.origLabel||(btn.dataset.origLabel=btn.textContent);
      btn.textContent=ok?'✓':'✗';
      btn.classList.toggle('copy-ok',ok);
      clearTimeout(revert);
      revert=setTimeout(function(){
        btn.textContent=orig;
        btn.classList.remove('copy-ok');
      },1200);
    }
    btn.addEventListener('click',function(ev){
      // On the home page the button sits next to a row-wide <a>; stop the
      // event before it can reach any ancestor handler and navigate.
      ev.preventDefault();ev.stopPropagation();
      if(navigator.clipboard&&navigator.clipboard.writeText){
        navigator.clipboard.writeText(text).then(function(){done(true)},function(){done(legacyCopy(text))});
        return;
      }
      done(legacyCopy(text));
    });
  });
})();
