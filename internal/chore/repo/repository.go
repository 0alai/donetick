package chore

import (
	"context"
	"errors"
	"fmt"
	"time"

	config "donetick.com/core/config"
	chModel "donetick.com/core/internal/chore/model"
	cModel "donetick.com/core/internal/circle/model"
	storageModel "donetick.com/core/internal/storage/model"
	stModel "donetick.com/core/internal/subtask/model"
	"gorm.io/gorm"
)

type ChoreRepository struct {
	db     *gorm.DB
	dbType string
}

func NewChoreRepository(db *gorm.DB, cfg *config.Config) *ChoreRepository {
	return &ChoreRepository{db: db, dbType: cfg.Database.Type}
}

func (r *ChoreRepository) UpsertChore(c context.Context, chore *chModel.Chore) error {
	return r.db.WithContext(c).Model(&chore).Save(chore).Error
}
func (r *ChoreRepository) UpdateChorePriority(c context.Context, userID int, choreID int, priority int) error {
	var affectedRows int64
	r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ? and created_by = ?", choreID, userID).Update("priority", priority).Count(&affectedRows)
	if affectedRows == 0 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *ChoreRepository) UpdateChoreFields(ctx context.Context, choreID int, fields map[string]interface{}) error {
	return r.db.WithContext(ctx).Model(&chModel.Chore{}).Where("id = ?", choreID).Updates(fields).Error
}

func (r *ChoreRepository) UpdateChores(c context.Context, chores []*chModel.Chore) error {
	return r.db.WithContext(c).Save(&chores).Error
}
func (r *ChoreRepository) CreateChore(c context.Context, chore *chModel.Chore) (int, error) {
	if err := r.db.WithContext(c).Create(chore).Error; err != nil {
		return 0, err
	}
	return chore.ID, nil
}

func (r *ChoreRepository) GetChore(c context.Context, choreID int) (*chModel.Chore, error) {
	var chore chModel.Chore
	if err := r.db.Debug().WithContext(c).Model(&chModel.Chore{}).Preload("SubTasks", "chore_id = ?", choreID).Preload("Assignees").Preload("ThingChore").Preload("LabelsV2").First(&chore, choreID).Error; err != nil {
		return nil, err
	}
	return &chore, nil
}

func (r *ChoreRepository) GetChores(c context.Context, circleID int, userID int, includeArchived bool) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	query := r.db.WithContext(c).Preload("Assignees").Preload("LabelsV2").Joins("left join chore_assignees on chores.id = chore_assignees.chore_id").Where("chores.circle_id = ? AND (chores.created_by = ? OR chore_assignees.user_id = ?)", circleID, userID, userID).Group("chores.id").Order("next_due_date asc")
	if !includeArchived {
		query = query.Where("chores.is_active = ?", true)
	}
	if err := query.Find(&chores, "circle_id = ?", circleID).Error; err != nil {
		return nil, err
	}
	return chores, nil
}

func (r *ChoreRepository) GetArchivedChores(c context.Context, circleID int, userID int) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	if err := r.db.WithContext(c).Preload("Assignees").Preload("LabelsV2").Joins("left join chore_assignees on chores.id = chore_assignees.chore_id").Where("chores.circle_id = ? AND (chores.created_by = ? OR chore_assignees.user_id = ?)", circleID, userID, userID).Group("chores.id").Order("next_due_date asc").Find(&chores, "circle_id = ? AND is_active = ?", circleID, false).Error; err != nil {
		return nil, err
	}
	return chores, nil
}
func (r *ChoreRepository) DeleteChore(c context.Context, id int) error {
	return r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("chore_id = ?", id).Delete(&chModel.ChoreAssignees{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&chModel.ChoreHistory{}, "chore_id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Delete(&chModel.Chore{}, id).Error; err != nil {
			return err
		}
		// Delete all subtasks associated with the chore
		if err := tx.Where("chore_id = ?", id).Delete(&stModel.SubTask{}).Error; err != nil {
			return err
		}
		// Delete all chore storage files associated with the chore:
		if err := tx.Where("entity_type = ? AND entity_id = ?", storageModel.EntityTypeChoreDescription, id).Delete(&storageModel.StorageFile{}).Error; err != nil {
			return err
		}

		return nil
	})
}

func (r *ChoreRepository) SoftDelete(c context.Context, id int, userID int) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ?", id).Where("created_by = ? ", userID).Update("is_active", false).Error

}

func (r *ChoreRepository) IsChoreOwner(c context.Context, choreID int, userID int) error {
	var chore chModel.Chore
	err := r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ? AND created_by = ?", choreID, userID).First(&chore).Error
	return err
}

func (r *ChoreRepository) CompleteChore(c context.Context, chore *chModel.Chore, note *string, userID int, dueDate *time.Time, completedDate *time.Time, nextAssignedTo int, applyPoints bool) error {
	err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {

		choreUpdates := map[string]interface{}{}
		choreUpdates["next_due_date"] = dueDate
		choreUpdates["status"] = chModel.ChoreStatusNoStatus

		if dueDate != nil {
			choreUpdates["assigned_to"] = nextAssignedTo
		} else {
			// one time task
			choreUpdates["is_active"] = false
		}
		// Create a new chore history record.
		ch := &chModel.ChoreHistory{
			ChoreID:     chore.ID,
			PerformedAt: completedDate,
			CompletedBy: userID,
			AssignedTo:  chore.AssignedTo,
			DueDate:     chore.NextDueDate,
			Note:        note,
			Status:      chModel.ChoreHistoryStatusCompleted,
		}

		// Update UserCirclee Points :
		if applyPoints && chore.Points != nil && *chore.Points > 0 {
			ch.Points = chore.Points
			if err := tx.Model(&cModel.UserCircle{}).Where("user_id = ? AND circle_id = ?", userID, chore.CircleID).Update("points", gorm.Expr("points + ?", chore.Points)).Error; err != nil {
				return err
			}
		}
		// Perform the update operation once, using the prepared updates map.
		if err := tx.Model(&chModel.Chore{}).Where("id = ?", chore.ID).Updates(choreUpdates).Error; err != nil {
			return err
		}

		if err := tx.Create(ch).Error; err != nil {
			return err
		}
		return nil
	})
	return err
}

func (r *ChoreRepository) SkipChore(c context.Context, chore *chModel.Chore, userID int, dueDate *time.Time, nextAssignedTo int) error {
	err := r.db.WithContext(c).Transaction(func(tx *gorm.DB) error {
		choreUpdates := map[string]interface{}{}
		choreUpdates["next_due_date"] = dueDate
		choreUpdates["status"] = chModel.ChoreStatusNoStatus

		if dueDate != nil {
			choreUpdates["assigned_to"] = nextAssignedTo
		} else {
			// one time task
			choreUpdates["is_active"] = false
		}

		// Create a new chore history record for the skipped chore
		skippedAt := time.Now().UTC()
		ch := &chModel.ChoreHistory{
			ChoreID:     chore.ID,
			PerformedAt: &skippedAt,
			CompletedBy: userID,
			AssignedTo:  chore.AssignedTo,
			DueDate:     chore.NextDueDate,
			Note:        nil,
			Status:      chModel.ChoreHistoryStatusSkipped,
		}

		// Perform the update operation once, using the prepared updates map.
		if err := tx.Model(&chModel.Chore{}).Where("id = ?", chore.ID).Updates(choreUpdates).Error; err != nil {
			return err
		}

		if err := tx.Create(ch).Error; err != nil {
			return err
		}
		return nil
	})
	return err
}

func (r *ChoreRepository) GetChoreHistory(c context.Context, choreID int) ([]*chModel.ChoreHistory, error) {
	var histories []*chModel.ChoreHistory
	if err := r.db.WithContext(c).Where("chore_id = ?", choreID).Order("performed_at desc").Find(&histories).Error; err != nil {
		return nil, err
	}
	return histories, nil
}
func (r *ChoreRepository) GetChoreHistoryWithLimit(c context.Context, choreID int, limit int) ([]*chModel.ChoreHistory, error) {
	var histories []*chModel.ChoreHistory
	if err := r.db.WithContext(c).Where("chore_id = ?", choreID).Order("performed_at desc").Limit(limit).Find(&histories).Error; err != nil {
		return nil, err
	}
	return histories, nil
}

func (r *ChoreRepository) GetChoreHistoryByID(c context.Context, choreID int, historyID int) (*chModel.ChoreHistory, error) {
	var history chModel.ChoreHistory
	if err := r.db.WithContext(c).Where("id = ? and chore_id = ? ", historyID, choreID).First(&history).Error; err != nil {
		return nil, err
	}
	return &history, nil
}

func (r *ChoreRepository) UpdateChoreHistory(c context.Context, history *chModel.ChoreHistory) error {
	return r.db.WithContext(c).Save(history).Error
}

func (r *ChoreRepository) DeleteChoreHistory(c context.Context, historyID int) error {
	return r.db.WithContext(c).Delete(&chModel.ChoreHistory{}, historyID).Error
}
func (r *ChoreRepository) DeleteChoreHistoryByChoreID(c context.Context, tx, choreID int) error {
	return r.db.WithContext(c).Delete(&chModel.ChoreHistory{}, "chore_id = ?", choreID).Error
}

func (r *ChoreRepository) UpdateChoreAssignees(c context.Context, assignees []*chModel.ChoreAssignees) error {
	return r.db.WithContext(c).Save(&assignees).Error
}

func (r *ChoreRepository) DeleteChoreAssignees(c context.Context, choreAssignees []*chModel.ChoreAssignees) error {
	return r.db.WithContext(c).Delete(&choreAssignees).Error
}

func (r *ChoreRepository) GetChoreAssignees(c context.Context, choreID int) ([]*chModel.ChoreAssignees, error) {
	var assignees []*chModel.ChoreAssignees
	if err := r.db.WithContext(c).Find(&assignees, "chore_id = ?", choreID).Error; err != nil {
		return nil, err
	}
	return assignees, nil
}

func (r *ChoreRepository) RemoveChoreAssigneeByCircleID(c context.Context, userID int, circleID int) error {
	return r.db.WithContext(c).Where("user_id = ? AND chore_id IN (SELECT id FROM chores WHERE circle_id = ? and created_by != ?)", userID, circleID, userID).Delete(&chModel.ChoreAssignees{}).Error
}

// func (r *ChoreRepository) getChoreDueToday(circleID int) ([]*chModel.Chore, error) {
// 	var chores []*Chore
// 	if err := r.db.WithContext(c).Where("next_due_date <= ?", time.Now().UTC()).Find(&chores).Error; err != nil {
// 		return nil, err
// 	}
// 	return chores, nil
// }

func (r *ChoreRepository) GetAllActiveChores(c context.Context) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	// query := r.db.WithContext(c).Table("chores").Joins("left join notifications n on n.chore_id = chores.id and n.scheduled_for < chores.next_due_date")
	// if err := query.Where("chores.is_active = ? and chores.notification = ? and (n.is_sent = ? or n.is_sent is null)", true, true, false).Find(&chores).Error; err != nil {
	// 	return nil, err
	// }
	return chores, nil
}

func (r *ChoreRepository) GetChoresForNotification(c context.Context) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	query := r.db.WithContext(c).Table("chores").Joins("left join notifications n on n.chore_id = chores.id and n.scheduled_for = chores.next_due_date and n.type = 1")
	if err := query.Where("chores.is_active = ? and chores.notification = ? and n.id is null", true, true).Find(&chores).Error; err != nil {
		return nil, err
	}
	return chores, nil
}

// func (r *ChoreReposity) GetOverdueChoresForNotification(c context.Context, overdueDuration time.Duration, everyDuration time.Duration, untilDuration time.Duration) ([]*chModel.Chore, error) {
// 	var chores []*chModel.Chore
// 	query := r.db.Debug().WithContext(c).Table("chores").Select("chores.*, MAX(n.created_at) as max_notification_created_at").Joins("left join notifications n on n.chore_id = chores.id and n.scheduled_for = chores.next_due_date and n.type = 2")
// 	if err := query.Where("chores.is_active = ? and chores.notification = ? and chores.next_due_date < ? and chores.next_due_date > ?", true, true, time.Now().Add(overdueDuration).UTC(), time.Now().Add(untilDuration).UTC()).Where(readJSONBooleanField(r.dbType, "chores.notification_meta", "nagging")).Having("MAX(n.created_at) is null or MAX(n.created_at) < ?", time.Now().Add(everyDuration).UTC()).Group("chores.id").Find(&chores).Error; err != nil {
// 		return nil, err
// 	}
// 	return chores, nil
// }

func (r *ChoreRepository) GetOverdueChoresForNotification(c context.Context, overdueFor time.Duration, everyDuration time.Duration, untilDuration time.Duration) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	now := time.Now().UTC()
	overdueTime := now.Add(-overdueFor)
	everyTime := now.Add(-everyDuration)
	untilTime := now.Add(-untilDuration)

	query := r.db.Debug().WithContext(c).
		Table("chores").
		Select("chores.*, MAX(n.created_at) as max_notification_created_at").
		Joins("left join notifications n on n.chore_id = chores.id and n.type = 2").
		Where("chores.is_active = ? AND chores.notification = ? AND chores.next_due_date < ? AND chores.next_due_date > ?", true, true, overdueTime, untilTime).
		Where(readJSONBooleanField(r.dbType, "chores.notification_meta", "nagging")).
		Group("chores.id").
		Having("MAX(n.created_at) IS NULL OR MAX(n.created_at) < ?", everyTime)

	if err := query.Find(&chores).Error; err != nil {
		return nil, err
	}

	return chores, nil
}

// a predue notfication is a notification send before the due date in 6 hours, 3 hours :
func (r *ChoreRepository) GetPreDueChoresForNotification(c context.Context, preDueDuration time.Duration, everyDuration time.Duration) ([]*chModel.Chore, error) {
	var chores []*chModel.Chore
	query := r.db.WithContext(c).Table("chores").Select("chores.*, MAX(n.created_at) as max_notification_created_at").Joins("left join notifications n on n.chore_id = chores.id and n.scheduled_for = chores.next_due_date and n.type = 3")
	if err := query.Where("chores.is_active = ? and chores.notification = ? and chores.next_due_date > ? and chores.next_due_date < ?", true, true, time.Now().UTC(), time.Now().Add(everyDuration*2).UTC()).Where(readJSONBooleanField(r.dbType, "chores.notification_meta", "predue")).Having("MAX(n.created_at) is null or MAX(n.created_at) < ?", time.Now().Add(everyDuration).UTC()).Group("chores.id").Find(&chores).Error; err != nil {
		return nil, err
	}
	return chores, nil
}

func readJSONBooleanField(dbType string, columnName string, fieldName string) string {
	if dbType == "postgres" {
		return fmt.Sprintf("(%s::json->>'%s')::boolean", columnName, fieldName)
	}
	return fmt.Sprintf("JSON_EXTRACT(%s, '$.%s')", columnName, fieldName)
}

func (r *ChoreRepository) SetDueDate(c context.Context, choreID int, dueDate time.Time) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ?", choreID).Updates(map[string]interface{}{
		"next_due_date": dueDate,
		"is_active":     true,
	}).Error
}

func (r *ChoreRepository) SetDueDateIfNotExisted(c context.Context, choreID int, dueDate time.Time) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ? and next_due_date is null and is_active = ?", choreID, true).Update("next_due_date", dueDate).Error
}

func (r *ChoreRepository) GetChoreDetailByID(c context.Context, choreID int, circleID int) (*chModel.ChoreDetail, error) {
	var choreDetail chModel.ChoreDetail
	if err := r.db.WithContext(c).
		Table("chores").
		Preload("Subtasks").
		Select(`
        chores.id, 
        chores.name,
		chores.description, 
        chores.frequency_type, 
        chores.next_due_date, 
        chores.assigned_to,
        chores.created_by,
		chores.priority,
		chores.completion_window,
        recent_history.last_completed_date,
		recent_history.notes,
        recent_history.last_assigned_to as last_completed_by,
        COUNT(chore_histories.id) as total_completed`).
		Joins("LEFT JOIN chore_histories ON chores.id = chore_histories.chore_id").
		Joins(`LEFT JOIN (
        SELECT 
            chore_id, 
            assigned_to AS last_assigned_to, 
            performed_at AS last_completed_date,
			notes
			
        FROM chore_histories
        WHERE (chore_id, performed_at) IN (
            SELECT chore_id, MAX(performed_at)
            FROM chore_histories
            GROUP BY chore_id
        )
    ) AS recent_history ON chores.id = recent_history.chore_id`).
		Where("chores.id = ? and chores.circle_id = ?", choreID, circleID).
		Group("chores.id, recent_history.last_completed_date, recent_history.last_assigned_to, recent_history.notes").
		First(&choreDetail).Error; err != nil {
		return nil, err

	}
	return &choreDetail, nil
}

func (r *ChoreRepository) ArchiveChore(c context.Context, choreID int, userID int) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ? and created_by = ?", choreID, userID).Update("is_active", false).Error
}

func (r *ChoreRepository) UnarchiveChore(c context.Context, choreID int, userID int) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ? and created_by = ?", choreID, userID).Update("is_active", true).Error
}

func (r *ChoreRepository) GetChoresHistoryByUserID(c context.Context, userID int, circleID int, days int, includeCircle bool) ([]*chModel.ChoreHistory, error) {

	var chores []*chModel.ChoreHistory
	since := time.Now().AddDate(0, 0, days*-1)
	query := r.db.WithContext(c).
		Table("chore_histories").
		Select("chore_histories.*, circles.id as circle_id").
		Joins("LEFT JOIN chores ON chore_histories.chore_id = chores.id").
		Joins("LEFT JOIN circles ON chores.circle_id = circles.id").
		Where("circles.id = ? AND chore_histories.performed_at > ?", circleID, since).
		Order("chore_histories.performed_at desc")

	if !includeCircle {
		query = query.Where("chore_histories.completed_by = ?", userID)
	}

	if err := query.Find(&chores).Error; err != nil {
		return nil, err
	}
	return chores, nil
}

func (r *ChoreRepository) UpdateChoreStatus(c context.Context, choreID int, userId int, status chModel.Status) error {
	return r.db.WithContext(c).Model(&chModel.Chore{}).Where("id = ?", choreID).Where("created_by = ? ", userId).Update("status", status).Error
}
