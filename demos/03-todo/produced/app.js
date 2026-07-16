async function addTodo() {
    const title = document.getElementById('todoTitle').value;
    const response = await fetch('/todos', {
        method: 'POST',
        headers: {
            'Content-Type': 'application/json',
        },
        body: JSON.stringify({ title }),
    });
    const data = await response.json();
    document.getElementById('todoTitle').value = '';
    loadTodos();
}

async function loadTodos() {
    const response = await fetch('/todos');
    const todos = await response.json();
    const list = document.getElementById('todoList');
    list.innerHTML = '';
    todos.forEach(todo => {
        const li = document.createElement('li');
        li.textContent = JSON.stringify(todo);
        list.appendChild(li);
    });
}

loadTodos();
