//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"github.com/go-ole/go-ole"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var gdi32 = syscall.NewLazyDLL("gdi32.dll")
var shell32 = syscall.NewLazyDLL("shell32.dll")
var sendMessage = user32.NewProc("SendMessageW")
var appUI *windowUI

const (
	idName = 101 + iota
	idMic
	idRefresh
	idTest
	idLevel
	idCorrection
	idRole
	idAddress
	idKey
	idNewKey
	idFolder
	idBrowse
	idPeople
	idTimer
	idConnect
	idRecord
	idStop
	idOpen
	idStatus
	idRecover
	idUpdate
)

type winClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Class                        *uint16
	SmallIcon                          uintptr
}
type winMessage struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	X, Y           int32
	Private        uint32
}
type rect struct{ Left, Top, Right, Bottom int32 }
type uiResult struct {
	err          error
	closing      bool
	tested       bool
	testDone     chan struct{}
	notice       string
	downloadPath string
}
type windowUI struct {
	padFiles               [padCount]string
	padLabels              [padCount]string
	lastPadStatus          string
	hwnd, font, titleFont  uintptr
	controls               map[int]uintptr
	devices                []inputDevice
	e                      *engine
	scale                  float64
	busy                   bool
	results                chan uiResult
	testCancel             context.CancelFunc
	testDone               chan struct{}
	lastStatus, lastPeople string
	role                   int
	connections            [2]struct{ address, key string }
}

func wide(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func call(proc string, args ...uintptr) uintptr {
	r, _, _ := user32.NewProc(proc).Call(args...)
	return r
}
func messageBox(hwnd uintptr, text string, flags uintptr) uintptr {
	return call("MessageBoxW", hwnd, uintptr(unsafe.Pointer(wide(text))), uintptr(unsafe.Pointer(wide("CoopRecord"))), flags)
}
func setText(hwnd uintptr, s string) { call("SetWindowTextW", hwnd, uintptr(unsafe.Pointer(wide(s)))) }
func getText(hwnd uintptr) string {
	n := call("GetWindowTextLengthW", hwnd)
	b := make([]uint16, n+1)
	call("GetWindowTextW", hwnd, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return syscall.UTF16ToString(b)
}
func enable(hwnd uintptr, on bool) {
	n := uintptr(0)
	if on {
		n = 1
	}
	call("EnableWindow", hwnd, n)
}
func selected(hwnd uintptr) int      { n, _, _ := sendMessage.Call(hwnd, 0x147, 0, 0); return int(int32(n)) }
func (u *windowUI) px(n int) uintptr { return uintptr(int(float64(n) * u.scale)) }
func (u *windowUI) control(id int, class, text string, style uint32, x, y, w, h int) uintptr {
	ex := uintptr(0)
	if class == "EDIT" {
		ex = 0x200
	}
	child := call("CreateWindowExW", ex, uintptr(unsafe.Pointer(wide(class))), uintptr(unsafe.Pointer(wide(text))), uintptr(style|0x50000000), u.px(x), u.px(y), u.px(w), u.px(h), u.hwnd, uintptr(id), 0, 0)
	sendMessage.Call(child, 0x30, u.font, 1)
	if id != 0 {
		u.controls[id] = child
	}
	return child
}
func (u *windowUI) label(text string, x, y, w int) { u.control(0, "STATIC", text, 0, x, y, w, 21) }
func (u *windowUI) button(id int, text string, x, y, w int) {
	u.control(id, "BUTTON", text, 0x10000, x, y, w, 28)
}
func (u *windowUI) edit(id int, text string, x, y, w int) {
	u.control(id, "EDIT", text, 0x10080, x, y, w, 24)
	sendMessage.Call(u.controls[id], 0xC5, 1024, 0)
}

func previousWordStart(text string, caret uint32) uint32 {
	units := utf16.Encode([]rune(text))
	left := utf16.Decode(units[:min(int(caret), len(units))])
	i := len(left)
	for i > 0 && unicode.IsSpace(left[i-1]) {
		i--
	}
	if i > 0 {
		word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || r == '_' }
		kind := word(left[i-1])
		for i > 0 && !unicode.IsSpace(left[i-1]) && word(left[i-1]) == kind {
			i--
		}
	}
	return uint32(len(utf16.Encode(left[:i])))
}

func (u *windowUI) editShortcut(hwnd, key uintptr) bool {
	if key != 'A' && key != 8 {
		return false
	}
	for _, id := range []int{idName, idCorrection, idAddress, idKey, idFolder} {
		if hwnd == 0 || hwnd != u.controls[id] {
			continue
		}
		if key == 'A' {
			sendMessage.Call(hwnd, 0xB1, 0, ^uintptr(0))
			return true
		} // EM_SETSEL
		var start, end uint32
		sendMessage.Call(hwnd, 0xB0, uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&end))) // EM_GETSEL
		if start == end {
			start = previousWordStart(getText(hwnd), start)
		}
		sendMessage.Call(hwnd, 0xB1, uintptr(start), uintptr(end))
		sendMessage.Call(hwnd, 0xC2, 1, uintptr(unsafe.Pointer(wide("")))) // EM_REPLACESEL, undoable
		return true
	}
	return false
}
func (u *windowUI) combo(id int, items []string, x, y, w int) {
	h := u.control(id, "COMBOBOX", "", 0x210003, x, y, w, 200)
	for _, item := range items {
		sendMessage.Call(h, 0x143, 0, uintptr(unsafe.Pointer(wide(item))))
	}
	sendMessage.Call(h, 0x14e, 0, 0)
}

func loadSettings() settings {
	home, _ := os.UserHomeDir()
	c := settings{Name: os.Getenv("USERNAME"), Folder: filepath.Join(home, "Documents", "CoopRecord"), Address: "0.0.0.0:" + defaultPort}
	if path, err := os.UserConfigDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(path, "CoopRecord", "settings.json")); err == nil {
			_ = json.Unmarshal(b, &c)
		}
	}
	c.Key = sessionKey()
	if c.Role == 1 {
		c.Key = ""
	} else {
		c.Role = 0
	}
	return c
}
func saveSettings(c settings) error {
	path, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(path, "CoopRecord")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	c.Key = ""
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), b, 0600)
}
func (u *windowUI) settings() (settings, error) {
	c := settings{Name: strings.TrimSpace(getText(u.controls[idName])), Address: strings.TrimSpace(getText(u.controls[idAddress])), Key: strings.TrimSpace(getText(u.controls[idKey])), Folder: getText(u.controls[idFolder])}
	c.Role = selected(u.controls[idRole])
	c.PadFiles = u.padFiles
	c.PadKeys = u.readPadKeys()
	c.PadKeysSet = true
	i := selected(u.controls[idMic])
	if i >= 0 && i < len(u.devices) {
		c.DeviceID = u.devices[i].ID
	}
	n, err := strconv.Atoi(getText(u.controls[idCorrection]))
	if err != nil {
		return c, fmt.Errorf("поправка должна быть целым числом миллисекунд")
	}
	c.CorrectionMS = n
	if _, _, err = net.SplitHostPort(c.Address); err != nil {
		return c, fmt.Errorf("адрес должен быть в формате IP:порт, например 26.1.2.3:%s", defaultPort)
	}
	return c, checkSettings(c)
}

func (u *windowUI) refreshDevices(wanted string) {
	d, err := inputDevices()
	if err != nil {
		messageBox(u.hwnd, "Не удалось получить микрофоны: "+err.Error(), 0x10)
		return
	}
	u.devices = d
	sendMessage.Call(u.controls[idMic], 0x14b, 0, 0)
	index := 0
	for i, item := range d {
		sendMessage.Call(u.controls[idMic], 0x143, 0, uintptr(unsafe.Pointer(wide(item.Name))))
		if item.ID == wanted {
			index = i
		}
	}
	if len(d) > 0 {
		sendMessage.Call(u.controls[idMic], 0x14e, uintptr(index), 0)
	} else {
		setText(u.controls[idStatus], "Микрофонов нет. Подключите устройство и нажмите «Обновить».")
	}
}
func (u *windowUI) stopTest() {
	if u.testCancel != nil {
		u.testCancel()
		<-u.testDone
		u.testCancel = nil
		setText(u.controls[idTest], "Проверить звук")
	}
}
func (u *windowUI) testMic() {
	if u.testCancel != nil {
		u.stopTest()
		return
	}
	i := selected(u.controls[idMic])
	if i < 0 || i >= len(u.devices) {
		messageBox(u.hwnd, "Выберите микрофон.", 0x30)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.testCancel = cancel
	u.testDone = make(chan struct{})
	done := u.testDone
	id := u.devices[i].ID
	setText(u.controls[idTest], "Остановить тест")
	go func() {
		ready := make(chan error, 1)
		err := captureAudio(ctx, id, ready, func(packet) error { return nil }, &u.e.level)
		close(done)
		u.results <- uiResult{err: err, tested: true, testDone: done}
	}()
}
func (u *windowUI) async(fn func() error, closing bool) {
	u.busy = true
	u.update()
	go func() { u.results <- uiResult{err: fn(), closing: closing} }()
}

func (u *windowUI) chooseFolder(title string) string {
	type browseInfo struct {
		Owner, Root     uintptr
		Display         *uint16
		Title           *uint16
		Flags           uint32
		Callback, Param uintptr
		Image           int32
	}
	var display [260]uint16
	bi := browseInfo{Owner: u.hwnd, Display: &display[0], Title: wide(title), Flags: 0x51}
	pidl, _, _ := shell32.NewProc("SHBrowseForFolderW").Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer ole.CoTaskMemFree(pidl)
	var path [260]uint16
	ok, _, _ := shell32.NewProc("SHGetPathFromIDListW").Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(path[:])
}
func (u *windowUI) command(id int, notification int) {
	if id == idRole && notification == 1 {
		role := selected(u.controls[idRole])
		if u.busy || role < 0 || role > 1 || role == u.role {
			return
		}
		u.connections[u.role].address = getText(u.controls[idAddress])
		u.connections[u.role].key = getText(u.controls[idKey])
		u.role = role
		setText(u.controls[idAddress], u.connections[role].address)
		setText(u.controls[idKey], u.connections[role].key)
		u.update()
		return
	}
	if notification != 0 || u.busy {
		return
	}
	if u.soundpadCommand(id) {
		return
	}
	switch id {
	case idUpdate:
		if u.e.snapshot().Mode != "" {
			return
		}
		u.stopTest()
		u.busy = true
		setText(u.controls[idUpdate], "Скачивание / проверка…")
		u.update()
		go func() {
			path, tag, err := checkAppUpdate()
			notice := "Установлена версия v" + appVersion + ".\r\nПоследний релиз на GitHub: " + tag + ".\r\nОбновление не требуется."
			if path != "" {
				notice = "Скачана версия " + tag + ". Контрольная сумма проверена.\r\n\r\nЗакройте CoopRecord, распакуйте ZIP и замените файлы приложения. Записи и настройки сохранятся.\r\n\r\n" + path
			}
			u.results <- uiResult{err: err, notice: notice, downloadPath: path}
		}()
	case idRefresh:
		u.stopTest()
		u.refreshDevices("")
	case idTest:
		u.testMic()
	case idNewKey:
		setText(u.controls[idKey], sessionKey())
	case idBrowse:
		if p := u.chooseFolder("Куда сохранять записи подкаста?"); p != "" {
			setText(u.controls[idFolder], p)
		}
	case idConnect:
		u.stopTest()
		if u.e.snapshot().Mode != "" {
			if u.e.snapshot().Recording && messageBox(u.hwnd, "Остановить запись и отключиться?", 0x24) != 6 {
				return
			}
			u.async(func() error { u.e.disconnect(); return nil }, false)
			return
		}
		c, err := u.settings()
		if err != nil {
			messageBox(u.hwnd, err.Error(), 0x30)
			return
		}
		host := selected(u.controls[idRole]) == 0
		u.async(func() error {
			if err := saveSettings(c); err != nil {
				return err
			}
			if host {
				return u.e.host(c)
			}
			return u.e.join(c)
		}, false)
	case idRecord:
		u.async(u.e.startRecording, false)
	case idStop:
		u.async(u.e.stopRecording, false)
	case idOpen:
		p := u.e.snapshot().Folder
		if p == "" {
			p = getText(u.controls[idFolder])
		}
		r, _, _ := shell32.NewProc("ShellExecuteW").Call(u.hwnd, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(p))), 0, 0, 1)
		if r <= 32 {
			messageBox(u.hwnd, "Не удалось открыть папку.", 0x10)
		}
	case idRecover:
		u.stopTest()
		if p := u.chooseFolder("Выберите папку с session.json и исходными .capture"); p != "" {
			u.async(func() error {
				_, err := exportSession(p)
				if err == nil {
					u.e.mu.Lock()
					u.e.lastFolder = p
					u.e.status = "WAV восстановлены: " + p
					u.e.mu.Unlock()
				}
				return err
			}, false)
		}
	}
	u.update()
}
func (u *windowUI) update() {
	for {
		select {
		case result := <-u.results:
			if result.notice != "" {
				setText(u.controls[idUpdate], "Обновить приложение")
				if result.err == nil {
					messageBox(u.hwnd, result.notice, 0x40)
					if result.downloadPath != "" {
						dir := filepath.Dir(result.downloadPath)
						shell32.NewProc("ShellExecuteW").Call(u.hwnd, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(dir))), 0, 0, 1)
					}
				}
			}
			if result.tested {
				if u.testCancel != nil && result.testDone == u.testDone {
					u.stopTest()
				}
			} else {
				u.busy = false
			}
			if result.err != nil {
				messageBox(u.hwnd, result.err.Error(), 0x10)
			}
			if result.closing {
				call("DestroyWindow", u.hwnd)
				return
			}
		default:
			goto drained
		}
	}
drained:
	v := u.e.snapshot()
	u.updateSoundpad(v)
	idle := v.Mode == "" && !u.busy
	host := selected(u.controls[idRole]) == 0
	for _, id := range []int{idName, idMic, idRefresh, idTest, idCorrection, idRole, idAddress, idKey} {
		enable(u.controls[id], idle)
	}
	for _, id := range []int{idNewKey, idFolder, idBrowse} {
		enable(u.controls[id], idle && host)
	}
	enable(u.controls[idConnect], !u.busy && !v.Exporting)
	enable(u.controls[idRecord], !u.busy && v.CanRecord)
	enable(u.controls[idStop], !u.busy && v.Recording && v.Mode == "host")
	enable(u.controls[idRecover], idle)
	enable(u.controls[idUpdate], idle)
	connect := "Создать сессию"
	if !host {
		connect = "Подключиться"
	}
	if v.Mode != "" {
		connect = "Отключиться"
	}
	setText(u.controls[idConnect], connect)
	sendMessage.Call(u.controls[idLevel], 0x402, uintptr(v.Level), 0)
	status := v.Status
	if v.Recording && v.Mode == "host" && !v.Exporting && !u.busy {
		status = "● Идёт запись. Остановите её для создания WAV-файлов."
	}
	if v.Error != "" {
		status += "\r\n" + v.Error
	}
	if status != u.lastStatus {
		setText(u.controls[idStatus], status)
		u.lastStatus = status
	}
	if v.People != u.lastPeople {
		setText(u.controls[idPeople], v.People)
		u.lastPeople = v.People
	}
	seconds := int(v.Elapsed.Seconds())
	setText(u.controls[idTimer], fmt.Sprintf("%02d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60))
}

func windowProc(hwnd uintptr, msg uint32, w, l uintptr) uintptr {
	u := appUI
	switch msg {
	case 0x312: // WM_HOTKEY
		if u != nil && !u.busy {
			u.soundpadCommand(int(w))
		}
		return 0
	case 0x111:
		if u != nil {
			u.command(int(w&0xffff), int((w>>16)&0xffff))
		}
		return 0
	case 0x113:
		if u != nil {
			u.update()
		}
		return 0
	case 0x10:
		if u == nil {
			break
		}
		if u.busy || u.e.snapshot().Exporting {
			messageBox(hwnd, "Дождитесь завершения текущей операции.", 0x40)
			return 0
		}
		if u.e.snapshot().Recording && messageBox(hwnd, "Остановить запись, сохранить файлы и выйти?", 0x24) != 6 {
			return 0
		}
		u.stopTest()
		u.async(func() error { u.e.disconnect(); return nil }, true)
		return 0
	case 2:
		for i := 0; i < padCount; i++ {
			call("UnregisterHotKey", hwnd, uintptr(idPadFirst+i))
		}
		call("PostQuitMessage", 0)
		return 0
	}
	return call("DefWindowProcW", hwnd, uintptr(msg), w, l)
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--recover" {
		_, err := exportSession(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	defer ole.CoUninitialize()
	call("SetProcessDPIAware")
	dc := call("GetDC", 0)
	dpi, _, _ := gdi32.NewProc("GetDeviceCaps").Call(dc, 88)
	call("ReleaseDC", 0, dc)
	if dpi == 0 {
		dpi = 96
	}
	// Fit the doubled-width window on smaller monitors while preserving its layout.
	fit := min(float64(dpi)/96, min(float64(call("GetSystemMetrics", 0)-40)/1440, float64(call("GetSystemMetrics", 1)-80)/690))
	dpi = uintptr(fit * 96)
	u := &windowUI{controls: make(map[int]uintptr), e: newEngine(captureAudio), results: make(chan uiResult, 16), scale: float64(dpi) / 96}
	appUI = u
	fontHeight := int32(-13 * int(dpi) / 96)
	u.font, _, _ = gdi32.NewProc("CreateFontW").Call(uintptr(fontHeight), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
	fontHeight = int32(-25 * int(dpi) / 96)
	u.titleFont, _, _ = gdi32.NewProc("CreateFontW").Call(uintptr(fontHeight), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 0, 0, uintptr(unsafe.Pointer(wide("Segoe UI"))))
	defer gdi32.NewProc("DeleteObject").Call(u.font)
	defer gdi32.NewProc("DeleteObject").Call(u.titleFont)
	instance, _, _ := kernel.NewProc("GetModuleHandleW").Call(0)
	cursor := call("LoadCursorW", 0, 32512)
	class := wide("CoopRecordMainWindow")
	wc := winClass{Size: uint32(unsafe.Sizeof(winClass{})), Proc: syscall.NewCallback(windowProc), Instance: instance, Cursor: cursor, Background: 16, Class: class}
	if call("RegisterClassExW", uintptr(unsafe.Pointer(&wc))) == 0 {
		messageBox(0, "Не удалось зарегистрировать окно.", 0x10)
		return
	}
	style := uint32(0x00c80000 | 0x00020000)
	r := rect{Right: int32(u.px(1440)), Bottom: int32(u.px(690))}
	call("AdjustWindowRectEx", uintptr(unsafe.Pointer(&r)), uintptr(style), 0, 0x10000)
	u.hwnd = call("CreateWindowExW", 0x10000, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(wide("CoopRecord v"+appVersion+" — запись подкаста"))), uintptr(style), 0x80000000, 0x80000000, uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0, 0, instance, 0)
	if u.hwnd == 0 {
		messageBox(0, "Не удалось создать окно.", 0x10)
		return
	}
	syscall.NewLazyDLL("comctl32.dll").NewProc("InitCommonControls").Call()
	title := u.control(0, "STATIC", "CoopRecord", 0, 22, 12, 350, 34)
	sendMessage.Call(title, 0x30, u.titleFont, 1)
	u.label("v"+appVersion, 403, 23, 80)
	u.button(idUpdate, "Обновить приложение", 505, 15, 195)
	u.label("Совместная запись подкаста  ·  WAV 48 кГц / 16 бит  ·  Windows x64", 24, 47, 680)
	u.control(0, "BUTTON", "1. Ваш звук", 7, 20, 78, 680, 143)
	c := loadSettings()
	u.createSoundpad(c)
	u.role = c.Role
	u.connections[0].address, u.connections[0].key = "0.0.0.0:"+defaultPort, sessionKey()
	u.connections[1].address = "26.0.0.1:" + defaultPort
	u.connections[c.Role].address, u.connections[c.Role].key = c.Address, c.Key
	u.label("Ваше имя:", 34, 106, 103)
	u.edit(idName, c.Name, 145, 101, 240)
	u.label("Поправка, мс:", 433, 106, 110)
	u.edit(idCorrection, strconv.Itoa(c.CorrectionMS), 554, 101, 128)
	u.label("Микрофон:", 34, 143, 104)
	u.combo(idMic, nil, 145, 137, 423)
	u.button(idRefresh, "Обновить", 578, 135, 105)
	u.button(idTest, "Проверить звук", 145, 178, 145)
	u.control(idLevel, "msctls_progress32", "Уровень микрофона", 0, 304, 183, 230, 18)
	u.label("Уровень сигнала", 550, 182, 140)
	u.control(0, "BUTTON", "2. Сессия", 7, 20, 232, 680, 193)
	u.label("Ваша роль:", 34, 261, 104)
	u.combo(idRole, []string{"Хост — сохраняет файлы", "Участник — отправляет звук"}, 145, 254, 310)
	sendMessage.Call(u.controls[idRole], 0x14e, uintptr(c.Role), 0)
	u.label("Адрес:", 34, 299, 100)
	u.edit(idAddress, c.Address, 145, 292, 310)
	u.label("IP RadminVPN : порт", 472, 298, 213)
	u.label("Ключ сессии:", 34, 336, 108)
	u.edit(idKey, c.Key, 145, 330, 310)
	u.button(idNewKey, "Новый ключ", 473, 328, 210)
	u.label("Папка хоста:", 34, 377, 108)
	u.edit(idFolder, c.Folder, 145, 371, 423)
	u.button(idBrowse, "Обзор…", 578, 369, 105)
	u.control(0, "BUTTON", "3. Участники и запись", 7, 20, 437, 680, 110)
	u.control(idPeople, "EDIT", "", 0x200844, 34, 461, 390, 69)
	timer := u.control(idTimer, "STATIC", "00:00:00", 0, 475, 461, 190, 36)
	sendMessage.Call(timer, 0x30, u.titleFont, 1)
	u.label("Начало и стоп — у хоста", 458, 506, 228)
	u.button(idConnect, "Создать сессию", 20, 561, 175)
	u.button(idRecord, "Начать запись", 205, 561, 170)
	u.button(idStop, "Стоп", 385, 561, 120)
	u.button(idOpen, "Открыть папку", 515, 561, 185)
	u.control(idStatus, "EDIT", "", 0x200844, 20, 601, 680, 46)
	u.button(idRecover, "Восстановить WAV…", 20, 656, 195)
	u.label("Для разговора используйте отдельный звонок и наушники.", 230, 662, 476)
	u.refreshDevices(c.DeviceID)
	if err := u.applyPadKeys(); err != nil {
		u.e.lastError = err.Error()
	}
	u.update()
	call("SetTimer", u.hwnd, 1, 150, 0)
	call("ShowWindow", u.hwnd, 1)
	call("UpdateWindow", u.hwnd)
	var m winMessage
	for {
		n := call("GetMessageW", uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(n) <= 0 {
			break
		}
		// Handle these before TranslateMessage so Ctrl+Backspace cannot insert DEL.
		if m.Message == 0x100 && call("GetKeyState", 0x11)&0x8000 != 0 && call("GetKeyState", 0x12)&0x8000 == 0 && u.editShortcut(m.Window, m.WParam) {
			continue
		}
		if call("IsDialogMessageW", u.hwnd, uintptr(unsafe.Pointer(&m))) == 0 {
			call("TranslateMessage", uintptr(unsafe.Pointer(&m)))
			call("DispatchMessageW", uintptr(unsafe.Pointer(&m)))
		}
	}
}
