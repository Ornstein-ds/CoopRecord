package main

import "strings"

// guidance describes the next useful action without changing session permissions.
// In particular, checking a microphone is recommended, never a recording gate.
type guidance struct {
	title, detail string
	action        string
}

func sessionGuidance(v viewState, host, busy bool) guidance {
	switch {
	case v.Exporting:
		return guidance{"Сохраняем запись", "Дождитесь сообщения «Сохранено». Приложение собирает отдельные дорожки и общий WAV-файл.", ""}
	case busy:
		return guidance{"Выполняем действие…", "Дождитесь завершения. Ваши поля остаются на месте; результат появится в статусе записи.", ""}
	case v.Recording && v.Mode == "host":
		return guidance{"Идёт запись", "Нажмите «Остановить и сохранить», когда закончите. После сохранения разговор продолжится.", "stop"}
	case v.Recording:
		return guidance{"Хост записывает разговор", "Продолжайте говорить. Хост остановит запись и сохранит файлы на своём компьютере.", ""}
	case v.Error != "":
		return guidance{"Проверьте сообщение об ошибке", "Подробности показаны под кнопками записи. Если WAV не собраны, сохранённые исходники можно обработать кнопкой «Восстановить WAV…» после отключения.", ""}
	case v.Mode == "guest":
		if !v.PadsReady {
			return guidance{"Загружаем звуки хоста", "Дождитесь завершения загрузки саундпада. Хост сможет начать запись, когда все компьютеры будут готовы.", ""}
		}
		return guidance{"Вы подключены · ожидаем хоста", "Можно разговаривать. Начало и остановка записи — у хоста; сохранять файлы на этом ПК не нужно.", ""}
	case v.Mode == "host" && !v.CanRecord:
		return guidance{"Готовим сессию к записи", v.RecordBlocked, ""}
	case v.Mode == "host" && v.Folder != "" && v.Error == "" && strings.HasPrefix(v.Status, "Сохранено:"):
		return guidance{"Запись сохранена", "Откройте папку с дорожками. Для следующего дубля нажмите «Начать запись» — он получит отдельную папку.", "open"}
	case v.Mode == "host" && v.PeerCount == 0:
		return guidance{"Пригласите участников", "Передайте им ваш IP из RadminVPN, порт и ключ сессии слева. 0.0.0.0 передавать не нужно. Можно записаться одному.", "record"}
	case v.Mode == "host":
		return guidance{"Все готовы к записи", "Нажмите «Начать запись». Общая запись начнётся через секунду; голосовая связь уже работает.", "record"}
	case host:
		return guidance{"Создайте сессию", "Проверьте микрофон и папку записи слева. Звуки саундпада можно выбрать заранее; пустые пады не мешают записи.", "connect"}
	default:
		return guidance{"Подключитесь к хосту", "Введите IP RadminVPN хоста с портом и его ключ сессии. После подключения голос включится автоматически.", "connect"}
	}
}
