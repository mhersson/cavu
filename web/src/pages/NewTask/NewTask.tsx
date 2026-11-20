import { NewTaskForm } from '../../components/NewTaskDialog.tsx'
import PageHead from '../../components/PageHead.tsx'
import './NewTask.css'

export default function NewTask() {
  return (
    <div className="page">
      <PageHead title="New task" />
      <NewTaskForm />
    </div>
  )
}
