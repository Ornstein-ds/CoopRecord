//go:build windows

package main

import "strings"

const (
	idGuideTitle = 150 + iota
	idGuideDetail
	idAddressLabel
	idAddressHint
	idRoleHint
	idHelp
	idCorrectionHelp
	idMicHint
	idSaveSettings
)

type formDraft struct {
	fields [5]string
	device string
	role   int
}

func (u *windowUI) draft() formDraft {
	d := formDraft{role: selected(u.controls[idRole])}
	for i, id := range []int{idName, idCorrection, idAddress, idKey, idFolder} {
		d.fields[i] = getText(u.controls[id])
	}
	if i := selected(u.controls[idMic]); i >= 0 && i < len(u.devices) {
		d.device = u.devices[i].ID
	}
	return d
}

func (u *windowUI) hasDraftChanges() bool {
	return u.draftReady && (u.draft() != u.savedDraft || u.readPadKeys() != u.savedPadKeys || u.padFiles != u.savedPadFiles)
}

func setChangedText(hwnd uintptr, text string) {
	if hwnd != 0 && getText(hwnd) != text {
		setText(hwnd, text)
	}
}

func show(hwnd uintptr, visible bool) {
	if hwnd == 0 {
		return
	}
	n := uintptr(0)
	if visible {
		n = 5
	}
	call("ShowWindow", hwnd, n)
}

func (u *windowUI) updateGuidance(v viewState, host bool) {
	g := sessionGuidance(v, host, u.busy)
	if v.Mode == "" && !u.busy {
		switch {
		case len(u.devices) == 0:
			g = guidance{"Подключите микрофон", "Микрофон не найден. Подключите устройство и нажмите «Обновить» в блоке «Ваш звук».", "refresh"}
		case strings.TrimSpace(getText(u.controls[idName])) == "":
			g = guidance{"Укажите ваше имя", "По этому имени хост и участники узнают вас в сессии.", ""}
		case u.testCancel != nil:
			g = guidance{"Проверка микрофона", "Произнесите несколько слов: индикатор слева должен двигаться. Затем нажмите «Завершить проверку». Звук не сохраняется.", "test"}
		case !u.micChecked:
			g = guidance{"1. Проверьте ваш звук", "Наденьте наушники, выберите микрофон и нажмите «Проверить звук». После проверки создайте сессию или подключитесь к хосту.", "test"}
		}
	}
	setChangedText(u.controls[idGuideTitle], g.title)
	setChangedText(u.controls[idGuideDetail], g.detail)
	primary := map[string]int{"connect": idConnect, "record": idRecord, "stop": idStop, "open": idOpen, "test": idTest, "refresh": idRefresh}[g.action]
	if primary != u.primaryAction {
		for _, id := range []int{idConnect, idRecord, idStop, idOpen, idTest, idRefresh} {
			style := uintptr(0) // BS_PUSHBUTTON / BS_DEFPUSHBUTTON
			if id == primary {
				style = 1
			}
			sendMessage.Call(u.controls[id], 0xF4, style, 1) // BM_SETSTYLE
		}
		u.primaryAction = primary
	}
	if host {
		setChangedText(u.controls[idRoleHint], "Записи сохраняются на вашем ПК")
		setChangedText(u.controls[idAddressLabel], "Слушать адрес:")
		setChangedText(u.controls[idAddressHint], "0.0.0.0 — все сети")
	} else {
		setChangedText(u.controls[idRoleHint], "Адрес и ключ пришлёт хост")
		setChangedText(u.controls[idAddressLabel], "Адрес хоста:")
		setChangedText(u.controls[idAddressHint], "IP RadminVPN : порт")
	}
	show(u.controls[idNewKey], host)
	show(u.controls[idRecord], host && !v.Recording)
	show(u.controls[idStop], host && v.Recording)
	show(u.controls[idOpen], host)
	enable(u.controls[idOpen], host && !u.busy && !v.Exporting && !v.Recording && v.Folder != "")
	enable(u.controls[idSaveSettings], v.Mode == "" && !u.busy && u.hasDraftChanges())
	if u.testCancel != nil && v.Level > 0 {
		u.micChecked = true
	}
	micHint := "Говорите — шкала покажет уровень"
	if u.micChecked {
		micHint = "Сигнал микрофона получен"
	}
	setChangedText(u.controls[idMicHint], micHint)
}

const connectionHelp = "Хост создаёт сессию и сохраняет дорожки всех участников. Участник подключается к хосту.\r\n\r\nОба компьютера должны быть в одной сети RadminVPN. Хост передаёт свой IP из RadminVPN, порт (обычно 47652) и ключ из поля «Ключ сессии». Адрес 0.0.0.0 — только для прослушивания, его не нужно передавать участникам.\r\n\r\nПосле подключения микрофон слышен остальным даже до начала записи. Используйте наушники. Запись запускает и останавливает хост.\r\n\r\nСаундпад необязателен. Звуки выбирает хост до создания сессии. Микшер меняет только громкость в ваших наушниках.\r\n\r\nНастройки сохраняются при подключении или кнопкой «Сохранить настройки». Ключ сессии не сохраняется между запусками."
