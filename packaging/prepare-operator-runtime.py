#!/usr/bin/env python3
"""Build a relocatable, dependency-complete operator runtime in GitHub Actions.
The workstation installs the resulting archive, not project dependencies.
"""
from pathlib import Path, PurePosixPath
import argparse
import hashlib
import io
import json
import os
import platform
import shutil
import stat
import subprocess
import tarfile
import urllib.request
import zipfile

HERE=Path(__file__).resolve().parent
LOCK=json.loads((HERE/'operator-runtime-sources.json').read_text())

def run(*args, **kwargs):
    return subprocess.run([str(x) for x in args],check=True,**kwargs)

def sha(path):
    with Path(path).open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()

def source(name,root):
    spec=LOCK['sources'][name]
    with urllib.request.urlopen(spec['url'],timeout=180) as response:
        raw=response.read(256*1024*1024+1)
    if len(raw)!=spec['size'] or hashlib.sha256(raw).hexdigest()!=spec['sha256']:
        raise ValueError('source identity mismatch: '+name)
    destination=root/'sources'/name
    destination.mkdir(parents=True)
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        roots={PurePosixPath(x.filename).parts[0] for x in archive.infolist() if x.filename}
        if len(roots)!=1:raise ValueError('source must have one root')
        prefix=next(iter(roots))+'/'
        total=0
        links=[]
        for info in archive.infolist():
            parts=PurePosixPath(info.filename)
            total+=info.file_size
            if parts.is_absolute() or '..' in parts.parts or total>512<<20:
                raise ValueError('unsafe source member')
            if info.is_dir():continue
            rel=info.filename.removeprefix(prefix)
            if rel==info.filename:continue
            path=destination.joinpath(*PurePosixPath(rel).parts);path.parent.mkdir(parents=True,exist_ok=True)
            if stat.S_ISLNK(info.external_attr>>16):
                link=archive.read(info).decode('utf-8')
                target=Path(os.path.normpath(str(path.parent/link)))
                if Path(link).is_absolute():raise ValueError('absolute source link')
                try:target.relative_to(destination)
                except ValueError:raise ValueError('source link escapes its pinned archive')
                links.append((path,link));continue
            path.write_bytes(archive.read(info));path.chmod(0o644)
        for path,link in links:os.symlink(link,path)
    return destination

def write(path,text):
    path.parent.mkdir(parents=True,exist_ok=True);path.write_text(text);path.chmod(0o755)

def python_wrapper(group,entry):
    return '''#!/bin/sh
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PYTHON="${ARTEX_PYTHON311:-$HERE/python-envs/nxc/bin/python}"
if [ ! -x "$PYTHON" ]; then PYTHON="/opt/homebrew/opt/python@3.11/bin/python3.11"; fi
[ -x "$PYTHON" ] || { echo 'Python 3.11 runtime is missing' >&2; exit 127; }
export PYTHONPATH="$HERE/operator-python/'''+group+'''${PYTHONPATH:+:$PYTHONPATH}"
export PYTHONDONTWRITEBYTECODE=1
exec "$PYTHON" -B "$HERE/sources/'''+entry+'''" "$@"
'''

def build(root):
    if os.environ.get('GITHUB_ACTIONS')!='true' or platform.system()!='Darwin' or platform.machine() not in ('arm64','aarch64'):
        raise RuntimeError('build only on an authorized native macOS cloud runner')
    root.mkdir(parents=True,exist_ok=True)
    one=source('oneforall',root);directory=source('dirsearch',root)
    # Build dependencies on the native cloud runner. Installation on the user's
    # machine is a file extraction; no pip or compiler is run there.
    for name,src in [('oneforall',one),('dirsearch',directory)]:
        target=root/'operator-python'/name;target.mkdir(parents=True)
        run(os.sys.executable,'-m','pip','install','--no-warn-script-location','--target',target,'-r',src/'requirements.txt')
    write(root/'oneforall',python_wrapper('oneforall','oneforall/oneforall.py'))
    write(root/'dirsearch',python_wrapper('dirsearch','dirsearch/dirsearch.py'))
    java=Path(os.environ['JAVA_HOME']);shutil.copytree(java,root/'java',symlinks=False)
    fx=root/'javafx/lib';fx.mkdir(parents=True)
    for spec in LOCK['javafx'].values():
        with urllib.request.urlopen(spec['url'],timeout=120) as response:raw=response.read(128*1024*1024+1)
        if len(raw)!=spec['size'] or hashlib.sha256(raw).hexdigest()!=spec['sha256']:raise ValueError('JavaFX artifact identity mismatch')
        (fx/spec['name']).write_bytes(raw)
    write(root/'javafx-java','''#!/bin/sh
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$HERE/java/bin/java" --module-path "$HERE/javafx/lib" --add-modules javafx.controls,javafx.fxml,javafx.web,javafx.swing --add-exports javafx.graphics/com.sun.javafx.application=ALL-UNNAMED --add-exports javafx.graphics/com.sun.glass.ui=ALL-UNNAMED --add-opens javafx.graphics/javafx.scene=ALL-UNNAMED "$@"
''')

    # GUI runtime and core are ordinary vendored tool resources, not the host's
    # application profile, keychain, shell list or personal Electron state.
    ant=source('antsword',root) # Exact vendored runtime dependencies are part of this pinned source archive.
    electron=root/'electron-build';electron.mkdir();(electron/'package.json').write_text(json.dumps({'private':True,'dependencies':{'electron':LOCK['electron_version']}}))
    run('npm','install','--no-audit','--no-fund',cwd=electron)
    dist=electron/'node_modules/electron/dist/Electron.app';app=root/'AntSword.app';shutil.copytree(dist,app,symlinks=True)
    resources=app/'Contents/Resources';shutil.copytree(ant,resources/'antsword',symlinks=True)
    core=resources/'antsword/app.js';text=core.read_text();old="protocol.registerStandardSchemes(['ant-views', 'ant-static', 'ant-src']);"
    if old not in text:raise ValueError('upstream protocol patch point drifted')
    text=text.replace(old,"protocol.registerSchemesAsPrivileged(['ant-views','ant-static','ant-src'].map(scheme=>({scheme,privileges:{standard:true,secure:true,supportFetchAPI:true,corsEnabled:true}})));")
    point='webPreferences: {'
    if point not in text:raise ValueError('upstream window patch point drifted')
    text=text.replace(point,point+'\n      contextIsolation: false,\n      enableRemoteModule: true,',1)
    core.write_text(text)
    bootstrap=resources/'app';bootstrap.mkdir();(bootstrap/'package.json').write_text('{"name":"artex-antsword","main":"main.js"}')
    (bootstrap/'main.js').write_text("""const {app}=require('electron');
const path=require('path');const fs=require('fs');
const state=process.env.ARTEX_ANTSWORD_DATA||path.join(process.resourcesPath,'../../../runtime/antsword');
fs.mkdirSync(state,{recursive:true});process.env.AS_WORKDIR=state;process.env.HOME=path.join(state,'home');fs.mkdirSync(process.env.HOME,{recursive:true});app.setPath('userData',state);
const dialog=require('electron').dialog;for(const key of ['showOpenDialog','showSaveDialog','showMessageBox']){const sync=dialog[key+'Sync'];if(sync){dialog[key]=function(...args){if(typeof args[args.length-1]==='function'){const callback=args.pop();const result=sync.apply(dialog,args);callback(result);return result;}return sync.apply(dialog,args)}}}
if(process.env.ARTEX_TOOL_SMOKE==='1'){
  let failed=false;process.on('uncaughtException',e=>{console.error(e.stack);app.exit(2)});
  app.on('browser-window-created',(_,window)=>{
    window.webContents.on('console-message',(_,level,message)=>{if(level>=3){console.error(message);failed=true}});
    window.webContents.once('did-finish-load',()=>{setTimeout(()=>{console.log('ARTEX_ANTSWORD_READY');app.exit(failed?3:0)},3000)});
  });setTimeout(()=>app.exit(4),45000);
}
require('../antsword/app.js');
""")
    write(root/'antsword','''#!/bin/sh
set -eu
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
export ARTEX_ANTSWORD_DATA="${ARTEX_ANTSWORD_DATA:-$HERE/runtime/antsword}"
exec "$HERE/AntSword.app/Contents/MacOS/Electron" "$@"
''')
    shutil.rmtree(electron)
    run('codesign','--force','--deep','--sign','-',app)
    tool_entries=[]
    for name,entry,underlying in [('subfinder','subfinder','subfinder'),('dnsx','dnsx','dnsx'),('ksubdomain','ksubdomain','ksubdomain'),('frpc','frpc','frpc'),('frps','frps','frps'),('oneforall','oneforall','sources/oneforall/oneforall.py'),('dirsearch','dirsearch','sources/dirsearch/dirsearch.py'),('antsword','antsword','AntSword.app/Contents/MacOS/Electron'),('java','java/bin/java','java/bin/java'),('javafx-java','javafx-java','java/bin/java')]:
        tool_entries.append({'name':name,'purpose':'完整二开工具包主机运行时：'+name,'execution':'host','platform':'darwin/arm64','path':'tools/'+entry,'sha256':sha(root/entry),'resolved_executable':'tools/'+underlying,'resolved_sha256':sha(root/underlying)})
    records=[]
    for p in sorted(root.rglob('*')):
        if p.is_symlink():records.append({'path':p.relative_to(root).as_posix(),'link':os.readlink(p)})
        elif p.is_file():records.append({'path':p.relative_to(root).as_posix(),'size':p.stat().st_size,'sha256':sha(p)})
    manifest={'schema':1,'platform':'darwin/arm64','python':'3.11','java_version':LOCK['java_version'],'sources':LOCK['sources'],'electron':LOCK['electron_version'],'files':records,'tools':tool_entries}
    (root/'operator-runtime-manifest.json').write_text(json.dumps(manifest,indent=2))
    return manifest

def smoke(root):
    env={**os.environ,'ARTEX_PYTHON311':os.sys.executable,'PYTHONDONTWRITEBYTECODE':'1'}
    run(root/'java/bin/java','-version')
    # Exercise the real aarch64 JavaFX native loader and WindowServer on the
    # cloud host. The supplied JARs' old x86-64 dylibs are not used on ARM64.
    smoke_source=root/'JavaFXSmoke.java'
    smoke_source.write_text('import javafx.application.Application;import javafx.application.Platform;import javafx.stage.Stage;public class JavaFXSmoke extends Application {public void start(Stage s){s.show();System.out.println("ARTEX_JAVAFX_READY");Platform.exit();}public static void main(String[] a){launch(a);}}')
    run(root/'java/bin/javac','--module-path',root/'javafx/lib','--add-modules','javafx.graphics',smoke_source)
    run(root/'javafx-java','-cp',root,'JavaFXSmoke',timeout=45)
    smoke_source.unlink();(root/'JavaFXSmoke.class').unlink()

    run(root/'dirsearch','--help',env=env)
    # OneForAll's top-level Fire help imports its dependencies but never starts
    # enumeration. Network-disabled execution is enforced by the workflow.
    run(root/'oneforall','--help',env=env)
    env['ARTEX_TOOL_SMOKE']='1';env['ARTEX_ANTSWORD_DATA']=str(root/'runtime/antsword-smoke')
    run(root/'antsword',env=env,timeout=60)

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--destination',type=Path,required=True);parser.add_argument('--smoke',action='store_true');args=parser.parse_args()
    if args.smoke:smoke(args.destination)
    else:print(json.dumps({'files':len(build(args.destination)['files']),'destination':str(args.destination)}))
